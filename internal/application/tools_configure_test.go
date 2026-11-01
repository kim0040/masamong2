package application_test

import "testing"

func TestToolsConfigureRejectsUnknownKeysWithoutRevision(t *testing.T) {
	svc := setupSvc(t)
	before := svc.Revision()
	r := exec(svc, "tools.configure", map[string]any{"id": "help.version", "limits": map[string]any{"per_user_per_day": 2}})
	if r.OK || r.Error == nil || r.Revision != 0 || svc.Revision() != before {
		t.Fatalf("unknown nested setting should be rejected without a revision: result=%+v revision=%d", r, svc.Revision())
	}
}
