package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/memory/corpus"
)

const rawImportLine = `{"observation_id":"raw-1","platform":"discord","collection_id":"c","source_room_id":"discord:g:c","native_message_id":"m1","actor":{"native_account_id":"u1","observed_label":"Alice"},"content":"hello","time":{"parsed":"2026-01-01T00:00:00Z"},"record_kind":"human_text"}` + "\n"

func TestImportEntryPointsSuccessFailureCancellationAndIdempotency(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	raw := filepath.Join(t.TempDir(), "raw.jsonl")
	if err := os.WriteFile(raw, []byte(rawImportLine), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := e.ImportRaw(raw, nil, nil)
	if err != nil || first.Inserted != 1 {
		t.Fatalf("ImportRaw = %+v err=%v", first, err)
	}
	again, err := e.ImportRaw(raw, nil, nil)
	if err != nil || again.Inserted != 0 || again.Duplicate != 1 {
		t.Fatalf("ImportRaw repeat = %+v err=%v", again, err)
	}
	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{broken}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ImportRaw(bad, nil, nil); err == nil {
		t.Fatal("ImportRaw accepted malformed input")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.ImportRawContext(ctx, raw, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("ImportRawContext cancellation error = %v", err)
	}

	csv := filepath.Join(t.TempDir(), "chat.csv")
	if err := os.WriteFile(csv, []byte("Date,User,Message\n2026-01-01 10:00:00,Alice,hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kakao, err := e.ImportKakaoCSV(csv, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale)
	if err != nil || kakao.Inserted != 1 {
		t.Fatalf("ImportKakaoCSV = %+v err=%v", kakao, err)
	}
	kakao, err = e.ImportKakaoCSV(csv, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale)
	if err != nil || kakao.Inserted != 0 || kakao.Duplicate != 1 {
		t.Fatalf("ImportKakaoCSV repeat = %+v err=%v", kakao, err)
	}
	if err := os.WriteFile(csv, []byte("not,a,Kakao export\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ImportKakaoCSV(csv, "c", "kakao:c", "C", "UTC", corpus.DefaultMarkerLocale); err == nil {
		t.Fatal("ImportKakaoCSV accepted an invalid header")
	}
}
