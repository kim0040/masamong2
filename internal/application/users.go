package application

import (
	"sort"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/privacy"
)

// users.list is the console's People page: one row per Discord user the instance keeps personal data
// for — consent decisions, personal settings, fortune and notice subscriptions, and whether they are an
// operator or blocked. It shows that someone subscribed, not what they told the bot (no birth data, no
// notice profile). Names come from the fortune profile or the latest captured message. Changes go
// through the Actions that own the data (privacy.consent, preferences.set, fortune.subscribe,
// notices.subscribe, access.policy).

type userRow struct {
	UserID   string                    `json:"user_id"`
	Name     string                    `json:"name,omitempty"`
	Consents map[string]map[string]any `json:"consents"`
	Prefs    *UserPrefs                `json:"prefs,omitempty"`
	Fortune  map[string]any            `json:"fortune,omitempty"`
	Notices  []map[string]any          `json:"notices"`
	Operator bool                      `json:"operator"`
	Blocked  bool                      `json:"blocked"`
	// Reminders counts the person's reminders still to come (proposed or scheduled)
	Reminders int    `json:"reminders"`
	Updated   string `json:"updated_at"`
}

func (s *Service) usersList(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	users := map[string]*userRow{}
	row := func(id, at string) *userRow {
		u := users[id]
		if u == nil {
			u = &userRow{UserID: id, Consents: map[string]map[string]any{}, Notices: []map[string]any{}}
			users[id] = u
		}
		if at > u.Updated {
			u.Updated = at
		}
		return u
	}
	// each table is read to the end before the next query: SQLite has one connection
	if rows, err := db.DB.Query(`SELECT user_id, scope, status, policy_version, updated_at FROM consent_state`); err == nil {
		for rows.Next() {
			var st privacy.State
			var id string
			if rows.Scan(&id, &st.Scope, &st.Status, &st.Version, &st.UpdatedAt) != nil {
				continue
			}
			p, _ := privacy.Get(st.Scope)
			row(id, st.UpdatedAt).Consents[st.Scope] = map[string]any{"status": st.Status, "version": st.Version, "allows": p.Allows(st), "stale": p.Stale(st), "updated_at": st.UpdatedAt}
		}
		rows.Close()
	}
	if rows, err := db.DB.Query(`SELECT user_id, locale, timezone, quiet_start, quiet_end, weather_region, updated_at FROM user_prefs`); err == nil {
		for rows.Next() {
			var p UserPrefs
			if rows.Scan(&p.UserID, &p.Locale, &p.Timezone, &p.QuietStart, &p.QuietEnd, &p.WeatherRegion, &p.UpdatedAt) == nil {
				row(p.UserID, p.UpdatedAt).Prefs = &p
			}
		}
		rows.Close()
	}
	if rows, err := db.DB.Query(`SELECT user_id, display_name, sub_time, sub_active, last_sent, updated_at FROM fortune_profiles`); err == nil {
		for rows.Next() {
			var id, name, at, last, updated string
			var active int
			if rows.Scan(&id, &name, &at, &active, &last, &updated) != nil {
				continue
			}
			u := row(id, updated)
			u.Fortune = map[string]any{"time": at, "active": active == 1, "last_sent": last}
			if name != "" {
				u.Name = name
			}
		}
		rows.Close()
	}
	if rows, err := db.DB.Query(`SELECT user_id, kind, delivery_time, active, last_delivery, updated_at FROM notice_subs`); err == nil {
		for rows.Next() {
			var id, kind, at, last, updated string
			var active int
			if rows.Scan(&id, &kind, &at, &active, &last, &updated) == nil {
				u := row(id, updated)
				u.Notices = append(u.Notices, map[string]any{"kind": kind, "time": at, "active": active == 1, "last_delivery": last})
			}
		}
		rows.Close()
	}
	if rows, err := db.DB.Query(`SELECT owner_id, COUNT(*), MAX(updated_at) FROM reminders WHERE ` + reminderLive + ` GROUP BY owner_id`); err == nil {
		for rows.Next() {
			var id, at string
			var n int
			if rows.Scan(&id, &n, &at) == nil {
				row(id, at).Reminders = n
			}
		}
		rows.Close()
	}
	acc := s.Doc().Access
	for _, id := range acc.Operators {
		row(id, "").Operator = true
	}
	for _, id := range acc.BlockedUsers {
		row(id, "").Blocked = true
	}
	var unnamed []string
	for id, u := range users {
		if u.Name == "" {
			unnamed = append(unnamed, id)
		}
	}
	if eng, err := s.MemoryEngine(); err == nil && len(unnamed) > 0 {
		if labels, err := eng.Store.AccountLabels("discord", unnamed); err == nil {
			for id, l := range labels {
				users[id].Name = l
			}
		}
	}
	list := make([]*userRow, 0, len(users))
	for _, u := range users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Updated != list[j].Updated {
			return list[i].Updated > list[j].Updated
		}
		return list[i].UserID < list[j].UserID
	})
	policies := []map[string]any{}
	for _, p := range privacy.Policies() {
		policies = append(policies, map[string]any{"scope": p.Scope, "version": p.Version, "opt_out": p.OptOut})
	}
	return action.OK(req, map[string]any{"users": list, "policies": policies,
		"access": map[string]any{"operators": nonNil(acc.Operators), "blocked_users": nonNil(acc.BlockedUsers), "guild_admins": acc.GuildAdminsAllowed()}})
}
