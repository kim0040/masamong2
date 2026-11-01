package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/privacy"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Purpose-scoped consent (internal/features/privacy explains the scopes). privacy.consent is the one
// Action: a person changes their own consent from Discord (the invoker's ID is the subject, nobody can
// decide for someone else there); the operator can look everything up in the console and record a
// withdrawal a person asked for in person. Every change is kept in consent_audit, which feature data
// deletion does not touch.

// featureDDL are tables added after the TiDB backend was disabled (storage.TiDBEnabled): SQLite only.
// If TiDB comes back they need TiDB DDL next to them.
var featureDDL = []string{
	`CREATE TABLE IF NOT EXISTS consent_state (
		user_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		status TEXT NOT NULL,
		policy_version TEXT NOT NULL DEFAULT '',
		granted_at TEXT NOT NULL DEFAULT '',
		withdrawn_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL,
		PRIMARY KEY(user_id, scope)
	)`,
	`CREATE TABLE IF NOT EXISTS consent_audit (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT NOT NULL,
		scope TEXT NOT NULL,
		status TEXT NOT NULL,
		policy_version TEXT NOT NULL,
		via TEXT NOT NULL,
		at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_consent_audit_user ON consent_audit(user_id, id)`,
}

func migrateFeatures(st *storage.Store) error {
	if !st.IsSQLite() {
		return nil
	}
	for _, q := range featureDDL {
		if _, err := st.DB.Exec(q); err != nil {
			return fmt.Errorf("feature tables: %w", err)
		}
	}
	return nil
}

// scopeWords are the words for each consent scope in every loaded language
// (privacy.scope.<scope>.aliases in the message catalogs).
func (s *Service) scopeWords() map[string]string {
	out := map[string]string{}
	for _, p := range privacy.Policies() {
		for _, loc := range s.I18n.Locales() {
			words, ok := s.I18n.Lookup(loc, "privacy.scope."+p.Scope+".aliases")
			if !ok {
				continue
			}
			for _, w := range strings.Split(words, ",") {
				if w = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(w), " ", "")); w != "" {
					out[w] = p.Scope
				}
			}
		}
	}
	return out
}

// memoryOptOuts caches who withdrew consent to being remembered (read on every captured message).
type optOutCache struct {
	mu     sync.RWMutex
	loaded bool
	ids    map[string]bool
}

var memoryOptOuts sync.Map // *Service → *optOutCache

func (s *Service) optOuts() *optOutCache {
	v, _ := memoryOptOuts.LoadOrStore(s, &optOutCache{})
	return v.(*optOutCache)
}

// MemoryAllowed reports whether a Discord account's messages may be stored and searched (the account
// has not withdrawn consent to being remembered).
func (s *Service) MemoryAllowed(accountID string) bool {
	if accountID == "" || s.Store == nil {
		return true
	}
	c := s.optOuts()
	c.mu.RLock()
	if c.loaded {
		out := c.ids[accountID]
		c.mu.RUnlock()
		return !out
	}
	c.mu.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.ids = map[string]bool{}
		if db, err := s.adminDB(); err == nil {
			rows, err := db.DB.Query(`SELECT user_id FROM consent_state WHERE scope=? AND status=?`, privacy.Memory, privacy.Withdrawn)
			if err == nil {
				for rows.Next() {
					var id string
					if rows.Scan(&id) == nil {
						c.ids[id] = true
					}
				}
				rows.Close()
				c.loaded = true
			}
		}
	}
	return !c.ids[accountID]
}

// ConsentAllows reports whether a feature may use a person's data under the current policy.
func (s *Service) ConsentAllows(userID, scope string) bool {
	p, ok := privacy.Get(scope)
	if !ok {
		return false
	}
	st, err := s.consentState(userID, scope)
	if err != nil {
		return false
	}
	return p.Allows(st)
}

func (s *Service) consentState(userID, scope string) (privacy.State, error) {
	db, err := s.adminDB()
	if err != nil {
		return privacy.State{}, err
	}
	st := privacy.State{Scope: scope}
	err = db.DB.QueryRow(`SELECT status, policy_version, granted_at, withdrawn_at, updated_at FROM consent_state WHERE user_id=? AND scope=?`,
		userID, scope).Scan(&st.Status, &st.Version, &st.GrantedAt, &st.WithdrawnAt, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	return st, err
}

// setConsent records a decision (and its audit row) and applies what it means right away.
func (s *Service) setConsent(userID, scope, status, via string) (privacy.State, error) {
	p, _ := privacy.Get(scope)
	db, err := s.adminDB()
	if err != nil {
		return privacy.State{}, err
	}
	if scope == privacy.Memory && status == privacy.Withdrawn {
		if _, err := store.Open(db); err != nil {
			return privacy.State{}, err
		}
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	err = db.WithWriter(func(tx *sql.Tx) error {
		if scope == privacy.Memory && status == privacy.Withdrawn {
			if err := s.withdrawalIntent(tx, userID); err != nil {
				return err
			}
		}
		granted, withdrawn := "", ""
		if status == privacy.Granted {
			granted = now
		} else {
			withdrawn = now
		}
		if _, err := tx.Exec(`INSERT INTO consent_state(user_id, scope, status, policy_version, granted_at, withdrawn_at, updated_at) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(user_id, scope) DO UPDATE SET status=excluded.status, policy_version=excluded.policy_version,
			granted_at=CASE WHEN excluded.granted_at<>'' THEN excluded.granted_at ELSE consent_state.granted_at END,
			withdrawn_at=CASE WHEN excluded.withdrawn_at<>'' THEN excluded.withdrawn_at ELSE consent_state.withdrawn_at END,
			updated_at=excluded.updated_at`, userID, scope, status, p.Version, granted, withdrawn, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO consent_audit(user_id, scope, status, policy_version, via, at) VALUES(?,?,?,?,?,?)`,
			userID, scope, status, p.Version, via, now)
		return err
	})
	if err != nil {
		return privacy.State{}, err
	}
	if scope == privacy.Memory {
		c := s.optOuts()
		c.mu.Lock()
		c.loaded = false // reread on the next check
		c.mu.Unlock()
	}
	s.emit("privacy", scope, map[string]any{"status": status})
	return s.consentState(userID, scope)
}

// decideConsent records a decision. Consent and its tombstone commit while late turn writes are
// serialized with the decision; existing records are cleared in resumable batches outside the lock.
func (s *Service) decideConsent(userID, scope, status, via string) (privacy.State, error) {
	s.privacyMu.Lock()
	defer s.privacyMu.Unlock()
	return s.setConsent(userID, scope, status, via)
}

// afterWithdraw deletes what a feature kept about a person who just withdrew its scope (memory has its
// own durable withdrawal job).
func (s *Service) afterWithdraw(ctx context.Context, userID, scope string) {
	switch scope {
	case privacy.Participation:
		// what collect mode recorded about them goes now: it was kept only with their agreement
		s.dropCollected(ctx, userID)
	case privacy.Profile:
		if err := s.dropProfile(ctx, userID); err != nil {
			s.Log.Warn("privacy: profile cards not deleted", "err", err.Error())
		}
	}
}

// privacyConsent is the privacy.consent Action.
//
//	{}                                  the invoker's states (Discord) / every recorded state (console)
//	{user_id}                           one person's states (console)
//	{scope, decision: grant|withdraw}   change a decision (Discord: always the invoker's own)
func (s *Service) privacyConsent(req action.Request) action.Result {
	return s.privacyConsentContext(context.Background(), req)
}

func (s *Service) privacyConsentContext(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	userID := strVal(in, "user_id", "")
	via := "console"
	if req.Invoker != nil && req.Invoker.UserID != "" {
		// from Discord a person only ever sees and changes their own consent
		userID, via = req.Invoker.UserID, "discord"
	}
	decision := strVal(in, "decision", "")
	scope := ""
	if raw := strVal(in, "scope", ""); raw != "" {
		sc, ok := privacy.Normalize(raw, s.scopeWords())
		if !ok {
			return action.Fail(req, errcode.InvalidInput, "privacy.scope_unknown", s.msg(req, "privacy.scope_unknown"), map[string]any{"scope": raw})
		}
		scope = sc
	}
	if decision == "" {
		if userID == "" {
			return s.consentList(req)
		}
		return s.consentOf(req, userID, scope)
	}
	if userID == "" || scope == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
	}
	var status string
	switch decision {
	case "grant":
		status = privacy.Granted
		if via == "console" {
			// agreeing is the person's own act: the operator can record a withdrawal, never a grant
			return action.Fail(req, errcode.PermissionDenied, "privacy.grant_self_only", s.msg(req, "privacy.grant_self_only"), nil)
		}
		if !boolVal(in, "confirmed") {
			// the person reads the policy first and agrees with its button (privacy:grant:<scope>)
			return s.consentNotice(req, userID, scope)
		}
	case "withdraw":
		status = privacy.Withdrawn
	default:
		return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
	}
	st, err := s.decideConsent(userID, scope, status, via)
	if err != nil {
		s.Log.Error("privacy: consent save failed", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	out := map[string]any{"user_id": userID, "scope": scope, "state": st, "changed": true}
	name := s.msg(req, "privacy.scope."+scope)
	card := action.Card{Title: s.msg(req, "privacy.card.granted", "scope", name), Ephemeral: true}
	if status == privacy.Withdrawn {
		card.Title, card.Tone = s.msg(req, "privacy.card.withdrawn", "scope", name), "neutral"
		card.Text = s.msg(req, "privacy.card.withdrawn_kept")
	}
	if scope == privacy.Memory && status == privacy.Withdrawn {
		// copies of their messages in turns, reviews and eval runs: queued before the messages go,
		// because copies that kept only text are found by it
		db, err := s.adminDB()
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), out)
		}
		n, err := s.processWithdrawal(ctx, db, redactionJob{User: userID, Ref: personRef(userID), At: s.Clock.Now().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), out)
		}
		out["removed_messages"] = n
		card.Text = s.msg(req, "privacy.card.memory_removed", "n", n)
		s.kickRedaction()
		if s.practice {
			s.drainRedactions(context.Background())
		}
		if err := s.applyPracticePrivacy(func(p *Service) error {
			raw, _ := json.Marshal(map[string]any{"user_id": userID, "scope": "memory", "decision": "withdraw"})
			res := p.privacyConsent(action.Request{Action: "privacy.consent", Input: raw, Locale: req.Locale})
			if !res.OK {
				return res.Error
			}
			return nil
		}); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), out)
		}
	}
	if status == privacy.Withdrawn {
		s.afterWithdraw(ctx, userID, scope)
	}
	if scope != privacy.Memory && status == privacy.Withdrawn {
		out["kept_data"] = true // the feature's data stays until its delete command; agreeing again resumes
	}
	return action.WithCard(action.OK(req, out), card)
}

// Practice is a second DB under this instance's root. Redact imported copies there too, reusing an
// active service so its index/cache is invalidated, or opening only the existing DB when it is idle.
// Unrelated instance roots and the operator's original import files are outside this ownership.
func (s *Service) applyPracticePrivacy(fn func(*Service) error) error {
	if s.practice {
		return nil
	}
	root := PracticeRoot(s.DataRoot)
	var target *Service
	memStates.Range(func(k, _ any) bool {
		p := k.(*Service)
		if p.practice && filepath.Clean(p.DataRoot) == filepath.Clean(root) {
			target = p
			return false
		}
		return true
	})
	if target != nil {
		if _, done, ok := target.beginWork(context.Background()); ok {
			defer done()
			return fn(target)
		}
	}
	path := filepath.Join(root, "masamong.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	st, err := storage.Open(path)
	if err != nil {
		return err
	}
	p := &Service{DataRoot: root, Store: st, Clock: s.Clock, I18n: s.I18n, Log: s.Log, practice: true, watchers: map[chan Event]struct{}{}}
	defer p.Close()
	return fn(p)
}

// consentNotice shows a policy and the button that records agreement to it.
func (s *Service) consentNotice(req action.Request, userID, scope string) action.Result {
	p, _ := privacy.Get(scope)
	st, _ := s.consentState(userID, scope)
	name := s.msg(req, "privacy.scope."+scope)
	out := map[string]any{"user_id": userID, "scope": scope, "needs_confirmation": !p.Allows(st) || p.OptOut, "state": st}
	card := action.Card{Title: s.msg(req, "privacy.card.notice_title", "scope", name, "version", p.Version), Text: s.msg(req, "privacy."+scope+".notice"),
		Tone: "neutral", Ephemeral: true}
	switch {
	case p.OptOut && st.Status == privacy.Withdrawn:
		// a withdrawn opt-out scope can be turned back on (LIVE-06)
		card.Buttons = []action.CardButton{{ID: "privacy:grant:" + scope, Label: s.msg(req, "privacy.card.agree"), Style: "success"}}
		card.Footer = s.msg(req, "privacy.card.regrant")
	case p.OptOut:
		// opt-out scopes need no agreement: they apply until withdrawn
		card.Footer = s.msg(req, "privacy.card."+scope+"_default")
	case p.Allows(st):
		card.Footer = s.msg(req, "privacy.card.already")
	default:
		card.Buttons = []action.CardButton{{ID: "privacy:grant:" + scope, Label: s.msg(req, "privacy.card.agree"), Style: "success"}}
		if p.Stale(st) {
			card.Footer = s.msg(req, "privacy.card.stale")
		}
	}
	return action.WithCard(action.OK(req, out), card)
}

func (s *Service) consentOf(req action.Request, userID, only string) action.Result {
	if only != "" && req.Invoker != nil {
		return s.consentNotice(req, userID, only)
	}
	var states []map[string]any
	card := action.Card{Title: s.msg(req, "privacy.card.status_title"), Tone: "neutral", Ephemeral: true, Footer: s.msg(req, "privacy.card.status_help")}
	for _, p := range privacy.Policies() {
		if only != "" && p.Scope != only {
			continue
		}
		st, err := s.consentState(userID, p.Scope)
		if err != nil {
			s.Log.Error("privacy: consent state read failed", "err", err)
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		states = append(states, map[string]any{"scope": p.Scope, "opt_out": p.OptOut, "current_version": p.Version,
			"state": st, "allows": p.Allows(st), "stale": p.Stale(st), "notice": s.msg(req, "privacy."+p.Scope+".notice")})
		label := "privacy.state.none"
		switch {
		case p.OptOut && st.Status != privacy.Withdrawn:
			label = "privacy.state.on_default"
		case p.Stale(st):
			label = "privacy.state.stale"
		case st.Status == privacy.Granted:
			label = "privacy.state.granted"
		case st.Status == privacy.Withdrawn:
			label = "privacy.state.withdrawn"
		}
		card.Fields = append(card.Fields, action.CardField{Name: s.msg(req, "privacy.scope."+p.Scope), Value: s.msg(req, label), Inline: true})
	}
	return action.WithCard(action.OK(req, map[string]any{"user_id": userID, "scopes": states}), card)
}

// consentList is every recorded decision (console), newest first, with recent audit rows.
func (s *Service) consentList(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		s.Log.Error("privacy: database unavailable", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	rows, err := db.DB.Query(`SELECT user_id, scope, status, policy_version, granted_at, withdrawn_at, updated_at FROM consent_state ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		s.Log.Error("privacy: consent list read failed", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	defer rows.Close()
	records := []map[string]any{}
	for rows.Next() {
		var user string
		var st privacy.State
		if err := rows.Scan(&user, &st.Scope, &st.Status, &st.Version, &st.GrantedAt, &st.WithdrawnAt, &st.UpdatedAt); err != nil {
			s.Log.Error("privacy: consent list scan failed", "err", err)
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		p, _ := privacy.Get(st.Scope)
		records = append(records, map[string]any{"user_id": user, "state": st, "allows": p.Allows(st), "stale": p.Stale(st)})
	}
	policies := []map[string]any{}
	for _, p := range privacy.Policies() {
		policies = append(policies, map[string]any{"scope": p.Scope, "version": p.Version, "opt_out": p.OptOut, "notice": s.msg(req, "privacy."+p.Scope+".notice")})
	}
	return action.OK(req, map[string]any{"records": records, "policies": policies})
}

// forgetPerson removes what the console's records kept of a person who withdrew memory consent: the
// text of their messages in the turn history and the participation log (collected rows hold no text but
// are about them, so they are deleted), and their reply ratings. The rest of each record (when, which channel, the outcome, the bot's reply) stays. Records written before
// speaker references were kept have none and go with retention.turn_detail. These are indexed updates;
// copies of their messages inside other people's turns are erased by the durable withdrawal job.
func (s *Service) forgetPerson(ctx context.Context, userID string) error {
	ref := personRef(userID)
	s.liveForget(userID)
	s.forgetThreadOwner(userID)
	db, err := s.adminDB()
	if err != nil || ref == "" {
		return err
	}
	if _, err := db.DB.Exec(`DELETE FROM reminder_context WHERE author_id=?`, userID); err != nil {
		s.Log.Warn("privacy: reminder context lines not deleted", "err", err.Error())
	}
	for _, q := range []string{
		`UPDATE turn_log SET body=json_set(body, '$.message', '', '$.speaker', '', '$.message_deleted', json('true')) WHERE rowid IN (SELECT rowid FROM turn_log WHERE speaker_ref=? AND (COALESCE(json_extract(body,'$.message'),'')<>'' OR COALESCE(json_extract(body,'$.speaker'),'')<>'' OR COALESCE(json_extract(body,'$.message_deleted'),0)=0) LIMIT ?)`,
		`DELETE FROM participation_log WHERE rowid IN (SELECT rowid FROM participation_log WHERE author_ref=? AND mode='collect' LIMIT ?)`,
		`UPDATE participation_log SET message='',draft='' WHERE rowid IN (SELECT rowid FROM participation_log WHERE author_ref=? AND (message<>'' OR draft<>'') LIMIT ?)`,
		`DELETE FROM turn_feedback WHERE rowid IN (SELECT rowid FROM turn_feedback WHERE user_hash=? LIMIT ?)`,
		`DELETE FROM conversation_log WHERE rowid IN (SELECT rowid FROM conversation_log WHERE author_ref=? LIMIT ?)`,
	} {
		if _, err := s.batched(ctx, func() (int64, error) {
			result, err := db.DB.ExecContext(ctx, q, ref, pruneBatch)
			if err != nil {
				return 0, err
			}
			return result.RowsAffected()
		}); err != nil {
			s.Log.Warn("privacy: record not cleared", "err", err.Error())
			return err
		}
	}
	return nil
}

// dropCollected deletes the rows collect mode recorded about a person (they hold no text, only IDs and
// features) when they withdraw participation consent.
func (s *Service) dropCollected(ctx context.Context, userID string) {
	db, err := s.adminDB()
	ref := personRef(userID)
	if err != nil || ref == "" {
		return
	}
	if _, err := s.batched(ctx, func() (int64, error) {
		result, err := db.DB.ExecContext(ctx, `DELETE FROM participation_log WHERE rowid IN (SELECT rowid FROM participation_log WHERE author_ref=? AND mode='collect' LIMIT ?)`, ref, pruneBatch)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}); err != nil {
		s.Log.Warn("privacy: collected rows not deleted", "err", err.Error())
	}
}
