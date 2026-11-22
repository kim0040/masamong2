package corpus

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rawLine(i int) string {
	return fmt.Sprintf(`{"observation_id":"raw-%d","platform":"discord","collection_id":"c","source_room_id":"discord:g:c","native_message_id":"m%d","actor":{"native_account_id":"u1","observed_label":"A"},"content":"hello","time":{"parsed":"2026-01-01T00:00:00Z"},"record_kind":"human_text"}`+"\n", i, i)
}

func writeGzip(t *testing.T, path string, lines int) (stored int64, unpacked int) {
	t.Helper()
	var plain bytes.Buffer
	for i := 0; i < lines; i++ {
		plain.WriteString(rawLine(i))
	}
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write(plain.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, packed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return int64(packed.Len()), plain.Len()
}

// A .gz can be small as stored and huge unpacked. The limit holds for the unpacked bytes too, the read
// stops there with ErrImportTooLarge (not a quiet end of file), and every row before the stop was
// already given to the caller, so the caller can say how much was imported before it failed.
func TestGzipImportStopsAtTheUnpackedLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.jsonl.gz")
	stored, unpacked := writeGzip(t, path, 400)
	limit := int64(unpacked / 4)
	if stored >= limit {
		t.Fatalf("test needs a file that is small stored (%d) and large unpacked (limit %d)", stored, limit)
	}
	rows := 0
	err := readRawCorpus(t.Context(), path, nil, limit, func(Observation) error { rows++; return nil })
	if !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("err = %v, want ErrImportTooLarge", err)
	}
	if rows == 0 || rows >= 400 {
		t.Fatalf("rows before the stop = %d, want some but not all", rows)
	}
	if max := int(limit)/len(rawLine(0)) + 1; rows > max {
		t.Fatalf("rows = %d, past what %d bytes hold (%d)", rows, limit, max)
	}
}

func TestGzipImportAtTheLimitIsAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.jsonl.gz")
	_, unpacked := writeGzip(t, path, 40)
	for _, limit := range []int64{int64(unpacked), int64(unpacked) + 1, 1 << 20} {
		rows := 0
		if err := readRawCorpus(t.Context(), path, nil, limit, func(Observation) error { rows++; return nil }); err != nil || rows != 40 {
			t.Fatalf("limit %d: rows=%d err=%v", limit, rows, err)
		}
	}
	rows := 0
	if err := readRawCorpus(t.Context(), path, nil, int64(unpacked)-1, func(Observation) error { rows++; return nil }); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("one byte over: rows=%d err=%v", rows, err)
	}
}

func TestPlainImportLimitStillApplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat(rawLine(1), 50)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readRawCorpus(t.Context(), path, nil, 1000, func(Observation) error { return nil }); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

// Only an ordinary file is opened: a pipe would block the open until someone writes to it, a
// directory and a device have no end.
func TestImportRefusesAFileThatIsNotOrdinary(t *testing.T) {
	dir := t.TempDir()
	if err := readRawCorpus(t.Context(), dir, nil, 1<<20, func(Observation) error { return nil }); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("directory: %v", err)
	}
	if _, _, err := ReadKakaoCSV(dir, "c", "kakao:r", func(Observation) error { return nil }); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("kakao directory: %v", err)
	}
	refuseFIFO(t, dir)
}

func TestImportFollowsALinkToAnOrdinaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "raw.jsonl")
	if err := os.WriteFile(target, []byte(rawLine(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	rows := 0
	if err := ReadRawCorpus(link, nil, func(Observation) error { rows++; return nil }); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
}
