package application_test

import (
	"strings"
	"testing"
)

func TestCRLFConfigDiffAndApplyAreNoOps(t *testing.T) {
	svc := setupSvc(t)
	show := exec(svc, "config.show", nil)
	if !show.OK {
		t.Fatalf("config.show: %+v", show.Error)
	}
	yaml := strings.ReplaceAll(strings.ReplaceAll(show.Output["yaml"].(string), "\r\n", "\n"), "\r", "\n")
	crlf := strings.ReplaceAll(yaml, "\n", "\r\n")
	diff := exec(svc, "config.diff", map[string]any{"yaml": crlf})
	if !diff.OK || diff.Output["changed"] != false || len(diff.Output["diff"].([]string)) != 0 {
		t.Fatalf("CRLF should have no diff: %+v", diff.Output)
	}
	base := svc.Revision()
	apply := exec(svc, "config.apply", map[string]any{"yaml": crlf, "base_revision": base})
	if !apply.OK || apply.Revision != base || apply.Output["applied"] != "unchanged" || svc.Revision() != base {
		t.Fatalf("CRLF no-op created a revision: result=%+v revision=%d", apply, svc.Revision())
	}
}
