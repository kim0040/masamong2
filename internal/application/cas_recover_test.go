package application_test

import (
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/config"
)

func TestPreparedCrashRecoversOrDiscards(t *testing.T) {
	svc := setupSvc(t)
	doc := config.Default("gate", svc.DataRoot)
	doc.I18n.Timezone = "UTC"
	raw, err := config.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	prep, err := svc.SimulatePreparedCrash(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if prep.Status != config.StatusPrepared {
		t.Fatalf("%+v", prep)
	}
	// File not updated → recover discards prepared and keeps revision 1
	if err := svc.RecoverAfterCrash(); err != nil {
		t.Fatal(err)
	}
	if svc.Revision() != 1 {
		t.Fatalf("rev %d", svc.Revision())
	}
	// Land the file then recover → commit
	prep2, err := svc.SimulatePreparedCrash(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.DataRoot, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = prep2
	if err := svc.RecoverAfterCrash(); err != nil {
		t.Fatal(err)
	}
	if svc.Revision() != 2 {
		t.Fatalf("expected commit to 2, got %d", svc.Revision())
	}
}
