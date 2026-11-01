package corpus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRawCorpusEntryPoints(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "raw.jsonl")
	line := `{"observation_id":"raw-1","platform":"discord","collection_id":"c","source_room_id":"discord:g:c","native_message_id":"m1","actor":{"native_account_id":"u1","observed_label":"Alice"},"content":"hello","time":{"parsed":"2026-01-01T00:00:00Z"},"record_kind":"human_text"}` + "\n"
	if err := os.WriteFile(good, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []Observation
	if err := ReadRawCorpus(good, nil, func(o Observation) error {
		got = append(got, o)
		return nil
	}); err != nil || len(got) != 1 || got[0].ObservationID != "raw-1" || got[0].Content != "hello" {
		t.Fatalf("ReadRawCorpus: %+v err=%v", got, err)
	}
	bad := filepath.Join(root, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{invalid}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReadRawCorpus(bad, nil, func(Observation) error { return nil }); err == nil {
		t.Fatal("malformed raw line was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ReadRawCorpusContext(ctx, good, nil, func(Observation) error { t.Fatal("canceled callback ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled raw read error = %v", err)
	}
}
