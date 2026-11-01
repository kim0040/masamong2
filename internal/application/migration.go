package application

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/notices"
	"masamong/internal/features/privacy"
)

// migration.apply (19장 A43): bring the previous bot's personal feature settings over — consent
// records, fortune profiles and subscriptions, school and transfer notice subscriptions — from a copy
// of its SQLite database. Conversation data is not part of it (memory starts from the raw archives,
// RAW_DATA_POLICY). The old database is opened read-only; what the new instance already has wins; and a
// person's data comes over only when they agreed to the policy version this instance asks for, which
// is why the consent versions were kept from the previous bot.
//
//	{path}                  the plan: what would come over and what would be skipped, nothing written
//	{path, confirm: true}   bring it over

type migrationPlan struct {
	Consents        int            `json:"consents"`
	Fortune         int            `json:"fortune_profiles"`
	SchoolNotices   int            `json:"school_notice_profiles"`
	TransferNotices int            `json:"transfer_subscriptions"`
	Skipped         map[string]int `json:"skipped"`
	Applied         bool           `json:"applied"`
}

func (s *Service) migrationApply(req action.Request) action.Result {
	in := inputMap(req)
	path := strings.TrimSpace(strVal(in, "path", ""))
	if path == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "path of the previous bot's SQLite database required", nil)
	}
	path, err := s.importPath(path)
	if err != nil {
		return action.Fail(req, errcode.PermissionDenied, "generic.error", err.Error(), nil)
	}
	if _, err := os.Stat(path); err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "database not found", nil)
	}
	old, err := sql.Open("sqlite", "file:"+url.PathEscape(path)+"?mode=ro&immutable=1")
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer old.Close()
	if err := old.Ping(); err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "not a SQLite database: "+err.Error(), nil)
	}
	plan, err := s.migrateV1(old, boolVal(in, "confirm"))
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), map[string]any{"plan": plan})
	}
	return action.OK(req, map[string]any{"plan": plan})
}

func hasTable(db *sql.DB, name string) bool {
	var n string
	return db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n) == nil
}

func (s *Service) migrateV1(old *sql.DB, apply bool) (migrationPlan, error) {
	plan := migrationPlan{Skipped: map[string]int{}}
	db, err := s.adminDB()
	if err != nil {
		return plan, err
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	// consent first: it decides what else may come over
	agreed := map[string]map[string]bool{} // scope → user → allowed now
	if hasTable(old, "privacy_consents") {
		rows, err := old.Query(`SELECT user_id, scope, policy_version, status, IFNULL(granted_at,''), IFNULL(withdrawn_at,''), updated_at FROM privacy_consents`)
		if err != nil {
			return plan, err
		}
		type consent struct{ user, scope, version, status, granted, withdrawn, updated string }
		var list []consent
		for rows.Next() {
			var c consent
			var uid int64
			if rows.Scan(&uid, &c.scope, &c.version, &c.status, &c.granted, &c.withdrawn, &c.updated) == nil {
				c.user = fmt.Sprint(uid)
				list = append(list, c)
			}
		}
		rows.Close()
		for _, c := range list {
			p, ok := privacy.Get(c.scope)
			if !ok {
				plan.Skipped["consent_unknown_scope"]++
				continue
			}
			st := privacy.State{Status: c.status, Version: c.version}
			if agreed[c.scope] == nil {
				agreed[c.scope] = map[string]bool{}
			}
			agreed[c.scope][c.user] = p.Allows(st)
			if cur, _ := s.consentState(c.user, c.scope); cur.Status != "" {
				plan.Skipped["consent_already_here"]++
				agreed[c.scope][c.user] = p.Allows(cur)
				continue
			}
			plan.Consents++
			if apply {
				if _, err := db.DB.Exec(`INSERT INTO consent_state(user_id, scope, status, policy_version, granted_at, withdrawn_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
					c.user, c.scope, c.status, c.version, c.granted, c.withdrawn, c.updated); err != nil {
					return plan, err
				}
				_, _ = db.DB.Exec(`INSERT INTO consent_audit(user_id, scope, status, policy_version, via, at) VALUES(?,?,?,?,?,?)`, c.user, c.scope, c.status, c.version, "migration", now)
			}
		}
		if apply {
			c := s.optOuts()
			c.mu.Lock()
			c.loaded = false
			c.mu.Unlock()
		}
	}
	allowed := func(scope, user string) bool { return agreed[scope][user] }

	if hasTable(old, "user_profiles") {
		rows, err := old.Query(`SELECT user_id, IFNULL(birth_date,''), IFNULL(birth_time,''), IFNULL(gender,''), IFNULL(birth_place,''),
			IFNULL(subscription_active,0), IFNULL(subscription_time,''), IFNULL(last_fortune_sent,'') FROM user_profiles`)
		if err != nil {
			return plan, err
		}
		type profile struct {
			user, date, at, gender, place, sub, last string
			active                                   int
		}
		var list []profile
		for rows.Next() {
			var p profile
			var uid int64
			if rows.Scan(&uid, &p.date, &p.at, &p.gender, &p.place, &p.active, &p.sub, &p.last) == nil {
				p.user = fmt.Sprint(uid)
				list = append(list, p)
			}
		}
		rows.Close()
		for _, p := range list {
			switch {
			case !dateRE.MatchString(p.date):
				plan.Skipped["fortune_no_birthday"]++
				continue
			case !allowed(privacy.Fortune, p.user):
				plan.Skipped["fortune_no_consent"]++
				continue
			}
			if _, exists := s.fortuneProfile(p.user); exists {
				plan.Skipped["fortune_already_here"]++
				continue
			}
			plan.Fortune++
			if !apply {
				continue
			}
			gender := map[string]string{"M": "male", "F": "female", "m": "male", "f": "female", "male": "male", "female": "female"}[p.gender]
			if !hhmm.MatchString(p.at) {
				p.at = ""
			}
			if !hhmm.MatchString(p.sub) {
				p.sub, p.active = "", 0
			}
			if _, err := db.DB.Exec(`INSERT INTO fortune_profiles(user_id, birth_date, birth_time, gender, birth_place, sub_time, sub_active, last_sent, created_at, updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?)`, p.user, p.date, p.at, gender, clip(p.place, 40), p.sub, p.active, p.last, now, now); err != nil {
				return plan, err
			}
		}
	}

	c, _, err := s.noticeCatalog()
	if err != nil {
		return plan, err
	}
	if hasTable(old, "school_notice_profiles") {
		rows, err := old.Query(`SELECT user_id, school_id, profile_json, enabled, IFNULL(delivery_time,'') FROM school_notice_profiles`)
		if err != nil {
			return plan, err
		}
		type sp struct {
			user, school, raw, at string
			enabled               int
		}
		var list []sp
		for rows.Next() {
			var p sp
			var uid int64
			if rows.Scan(&uid, &p.school, &p.raw, &p.enabled, &p.at) == nil {
				p.user = fmt.Sprint(uid)
				list = append(list, p)
			}
		}
		rows.Close()
		for _, p := range list {
			if !allowed(privacy.SchoolNotice, p.user) {
				plan.Skipped["school_no_consent"]++
				continue
			}
			if c.SchoolName(p.school) == "" {
				plan.Skipped["school_not_in_catalog"]++
				continue
			}
			if _, exists := s.noticeSub(p.user, notices.KindSchool); exists {
				plan.Skipped["school_already_here"]++
				continue
			}
			var v1 struct {
				Degree     string   `json:"degree_level"`
				Grade      int      `json:"grade"`
				Department string   `json:"department"`
				Campus     string   `json:"campus"`
				Preferred  []string `json:"preferred_topics"`
				Muted      []string `json:"muted_topics"`
			}
			if err := json.Unmarshal([]byte(p.raw), &v1); err != nil {
				plan.Skipped["school_unreadable"]++
				continue
			}
			topics := func(words []string) []string {
				var out []string
				for _, w := range words {
					if id, ok := c.TopicID(w); ok && !contains(out, id) {
						out = append(out, id)
					}
				}
				return out
			}
			degree := v1.Degree
			if _, ok := c.Vocabulary.DegreeAudiences[degree]; !ok {
				degree = "undergraduate"
			}
			plan.SchoolNotices++
			if apply {
				sub := noticeSub{UserID: p.user, Kind: notices.KindSchool, Active: p.enabled == 1,
					Profile: notices.Profile{School: p.school, Degree: degree, Grade: v1.Grade, Department: v1.Department, Campus: v1.Campus,
						Interests: topics(v1.Preferred), Muted: topics(v1.Muted)}}
				if hhmm.MatchString(p.at) {
					sub.DeliveryTime = p.at
				}
				if err := s.saveNoticeSub(sub); err != nil {
					return plan, err
				}
			}
		}
	}
	if hasTable(old, "transfer_notice_subscriptions") {
		rows, err := old.Query(`SELECT user_id, schools_json, enabled, IFNULL(delivery_time,'') FROM transfer_notice_subscriptions`)
		if err != nil {
			return plan, err
		}
		type ts struct {
			user, raw, at string
			enabled       int
		}
		var list []ts
		for rows.Next() {
			var t ts
			var uid int64
			if rows.Scan(&uid, &t.raw, &t.enabled, &t.at) == nil {
				t.user = fmt.Sprint(uid)
				list = append(list, t)
			}
		}
		rows.Close()
		for _, t := range list {
			if !allowed(privacy.TransferNotice, t.user) {
				plan.Skipped["transfer_no_consent"]++
				continue
			}
			if _, exists := s.noticeSub(t.user, notices.KindTransfer); exists {
				plan.Skipped["transfer_already_here"]++
				continue
			}
			var ids []string
			if err := json.Unmarshal([]byte(t.raw), &ids); err != nil {
				plan.Skipped["transfer_unreadable"]++
				continue
			}
			var sources []string
			for _, id := range ids {
				if _, ok := c.Source("transfer_" + id); ok {
					sources = append(sources, "transfer_"+id)
				}
			}
			if len(sources) == 0 {
				plan.Skipped["transfer_no_known_university"]++
				continue
			}
			plan.TransferNotices++
			if apply {
				sub := noticeSub{UserID: t.user, Kind: notices.KindTransfer, Sources: sources, Active: t.enabled == 1}
				if hhmm.MatchString(t.at) {
					sub.DeliveryTime = t.at
				}
				if err := s.saveNoticeSub(sub); err != nil {
					return plan, err
				}
			}
		}
	}
	plan.Applied = apply
	if !apply && plan.Consents+plan.Fortune+plan.SchoolNotices+plan.TransferNotices == 0 && len(plan.Skipped) == 0 {
		return plan, errors.New("nothing of the previous bot's feature tables was found in this database")
	}
	return plan, nil
}
