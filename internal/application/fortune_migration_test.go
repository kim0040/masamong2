package application_test

import (
	"database/sql"
	"testing"
)

func TestMigrationSkipsBirthDatesThatAreNotDates(t *testing.T) {
	// V1 kept a yearless birthday as 0000-MM-DD and did not check the calendar: neither becomes a profile
	svc := setupSvc(t)
	path := v1Database(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO privacy_consents VALUES (333, 'fortune', '2026-07-28.v1', 'h', 'granted', '2026-08-01', NULL, '2026-08-01')`,
		`INSERT INTO privacy_consents VALUES (444, 'fortune', '2026-07-28.v1', 'h', 'granted', '2026-08-01', NULL, '2026-08-01')`,
		`INSERT INTO user_profiles(user_id, birth_date) VALUES (333, '0000-05-15')`,
		`INSERT INTO user_profiles(user_id, birth_date) VALUES (444, '1990-02-30')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	plan := exec(svc, "migration.apply", map[string]any{"path": path})
	p := asMap(t, plan.Output)["plan"].(map[string]any)
	if p["fortune_profiles"].(float64) != 1 {
		t.Fatalf("plan: %+v", p)
	}
	if skipped := p["skipped"].(map[string]any); skipped["fortune_no_birthday"].(float64) != 2 {
		t.Fatalf("skipped: %v", skipped)
	}
}
