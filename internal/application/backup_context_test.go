package application_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/action"
)

func TestBackupActionHonorsCancellationBeforeCreatingFiles(t *testing.T) {
	svc := setupSvc(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := svc.Execute(ctx, action.Request{Action: "backup.create", Locale: "en"})
	if res.OK {
		t.Fatal("backup.create succeeded after its context was cancelled")
	}
	entries, err := os.ReadDir(filepath.Join(svc.DataRoot, "backups"))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled backup left files: %v", entries)
	}
}
