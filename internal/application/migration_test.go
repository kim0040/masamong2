package application_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func v1Database(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "masamong_v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE user_profiles (user_id INTEGER PRIMARY KEY, birth_date TEXT, birth_time TEXT, gender TEXT, is_lunar BOOLEAN, subscription_active BOOLEAN,
			subscription_time TEXT, pending_payload TEXT, last_fortune_sent TEXT, last_fortune_content TEXT, birth_place TEXT, created_at TEXT)`,
		`CREATE TABLE privacy_consents (user_id INTEGER, scope TEXT, policy_version TEXT, notice_hash TEXT, status TEXT, granted_at TEXT, withdrawn_at TEXT, updated_at TEXT, PRIMARY KEY(user_id, scope))`,
		`CREATE TABLE school_notice_profiles (user_id INTEGER PRIMARY KEY, user_key TEXT, school_id TEXT, profile_json TEXT, profile_version INTEGER, enabled INTEGER, delivery_time TEXT, created_at TEXT, updated_at TEXT)`,
		`CREATE TABLE transfer_notice_subscriptions (user_id INTEGER PRIMARY KEY, schools_json TEXT, enabled INTEGER, delivery_time TEXT, created_at TEXT, updated_at TEXT)`,
		`INSERT INTO privacy_consents VALUES (111, 'fortune', '2026-07-28.v1', 'h', 'granted', '2026-08-01', NULL, '2026-08-01')`,
		`INSERT INTO privacy_consents VALUES (222, 'fortune', '2026-07-28.v1', 'h', 'withdrawn', '2026-08-01', '2026-08-02', '2026-08-02')`,
		`INSERT INTO privacy_consents VALUES (111, 'school_notice', '2026-07-28.v2', 'h', 'granted', '2026-08-01', NULL, '2026-08-01')`,
		`INSERT INTO privacy_consents VALUES (111, 'transfer_notice', '2026-07-28.v1', 'h', 'granted', '2026-08-01', NULL, '2026-08-01')`,
		`INSERT INTO user_profiles(user_id, birth_date, birth_time, gender, subscription_active, subscription_time, birth_place) VALUES (111, '1990-01-01', '14:30', 'M', 1, '07:30', '서울')`,
		`INSERT INTO user_profiles(user_id, birth_date, gender) VALUES (222, '1991-02-02', 'F')`,
		`INSERT INTO school_notice_profiles VALUES (111, 'discord-111', 'jbnu', '{"school_id":"jbnu","degree_level":"undergraduate","grade":3,"department":"소프트웨어공학과","preferred_topics":["장학"],"muted_topics":["병무"]}', 1, 1, '09:00', '', '')`,
		`INSERT INTO transfer_notice_subscriptions VALUES (111, '["kangwon","nowhere"]', 1, '10:00', '', '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return path
}

func TestMigrationBringsConsentedSettingsOver(t *testing.T) {
	svc := setupSvc(t)
	path := v1Database(t)
	plan := exec(svc, "migration.apply", map[string]any{"path": path})
	p := asMap(t, plan.Output)["plan"].(map[string]any)
	if !plan.OK || p["applied"] != false || p["consents"].(float64) != 4 || p["fortune_profiles"].(float64) != 1 || p["school_notice_profiles"].(float64) != 1 || p["transfer_subscriptions"].(float64) != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	if skipped := p["skipped"].(map[string]any); skipped["fortune_no_consent"].(float64) != 1 {
		t.Fatalf("a withdrawn person's profile stays behind: %v", skipped)
	}
	if svc.ConsentAllows("111", "fortune") {
		t.Fatal("the plan writes nothing")
	}
	done := exec(svc, "migration.apply", map[string]any{"path": path, "confirm": true})
	if !done.OK {
		t.Fatalf("apply: %+v", done)
	}
	if !svc.ConsentAllows("111", "fortune") || svc.ConsentAllows("222", "fortune") {
		t.Fatal("consent came over as it was")
	}
	u := person("111", true)
	st := as(svc, "fortune.subscribe", map[string]any{"op": "status"}, u)
	if st.Output["subscribed"] != true || st.Output["time"] != "07:30" {
		t.Fatalf("fortune profile: %+v", st.Output)
	}
	prof := as(svc, "notices.status", map[string]any{"kind": "school", "what": "profile"}, u)
	if !prof.OK || prof.Output["status"] != "ok" {
		t.Fatalf("school profile: %+v", prof)
	}
	again := exec(svc, "migration.apply", map[string]any{"path": path, "confirm": true})
	if q := asMap(t, again.Output)["plan"].(map[string]any); q["consents"].(float64) != 0 || q["fortune_profiles"].(float64) != 0 {
		t.Fatalf("running it twice brings nothing twice: %v", q)
	}
	if r := exec(svc, "migration.apply", map[string]any{"path": filepath.Join(t.TempDir(), "none.db")}); r.OK {
		t.Fatal("a missing file is refused")
	}
}
