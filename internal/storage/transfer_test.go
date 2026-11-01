package storage

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// The MySQL text protocol hands back every scalar as []byte while SQLite returns typed values. The
// same logical row must convert and hash identically from both, or a verified copy would be a lie.
func TestConvertMatchesAcrossDrivers(t *testing.T) {
	cases := []struct {
		sqliteType, mysqlType string
		fromSQLite, fromMySQL any
	}{
		{"INTEGER", "BIGINT", int64(42), []byte("42")},
		{"INTEGER", "TINYINT", int64(1), []byte("1")},
		{"REAL", "DOUBLE", 0.0012345678901, []byte("0.0012345678901")},
		{"TEXT", "MEDIUMTEXT", "철수: 부산 가자 🐶", []byte("철수: 부산 가자 🐶")},
		{"TEXT", "VARCHAR", "", []byte("")},
		{"BLOB", "MEDIUMBLOB", []byte{0, 1, 255}, []byte{0, 1, 255}},
		{"BLOB", "MEDIUMBLOB", nil, nil},
	}
	for _, c := range cases {
		ks, km := kindOf(c.sqliteType), kindOf(c.mysqlType)
		if ks != km {
			t.Fatalf("%s vs %s: kinds %d/%d", c.sqliteType, c.mysqlType, ks, km)
		}
		a, err := convert(c.fromSQLite, ks)
		if err != nil {
			t.Fatal(err)
		}
		b, err := convert(c.fromMySQL, km)
		if err != nil {
			t.Fatal(err)
		}
		if canonical(a) != canonical(b) {
			t.Fatalf("%s: %q != %q", c.mysqlType, canonical(a), canonical(b))
		}
	}
}

func TestTransferRefusesNonEmptyTargetAndVerifies(t *testing.T) {
	dir := t.TempDir()
	src, err := Open(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := Open(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	for i, s := range []*Store{src, dst} {
		if _, err := s.DB.Exec(`INSERT INTO traces(id, body, created_at) VALUES(?,?,?)`, "t1", "{}", "2026-09-23T00:00:00Z"); err != nil {
			t.Fatal(i, err)
		}
	}
	ctx := context.Background()
	plan, err := PlanTransfer(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Ready() || len(plan.NotEmpty) != 1 || plan.NotEmpty[0] != "traces" {
		t.Fatalf("non-empty target must block: %+v", plan)
	}
	if _, err := Transfer(ctx, src, dst, nil); err == nil {
		t.Fatal("transfer into a non-empty target must fail")
	}
	if _, err := dst.DB.Exec(`DELETE FROM traces`); err != nil {
		t.Fatal(err)
	}
	var calls int
	plan, err = Transfer(ctx, src, dst, func(string, int64, int64) { calls++ })
	if err != nil || !plan.Verified() || calls == 0 {
		t.Fatalf("transfer: %+v %v calls=%d", plan, err, calls)
	}
	dump, err := dst.ExportLogical()
	if err != nil || len(dump) == 0 {
		t.Fatalf("logical dump: %v", err)
	}
}

func TestLogicalDumpRoundTripKeepsBlobs(t *testing.T) {
	ctx := context.Background()
	src, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.DB.Exec(`CREATE TABLE vecs (id TEXT PRIMARY KEY, n INTEGER, f REAL, v BLOB)`); err != nil {
		t.Fatal(err)
	}
	blob := []byte{0, 0xff, 0x80, 0x10, 0xc3}
	if _, err := src.DB.Exec(`INSERT INTO vecs VALUES ('x', 7, 1.5, ?), ('y', NULL, 0, NULL)`, blob); err != nil {
		t.Fatal(err)
	}
	raw, err := src.ExportLogical()
	if err != nil {
		t.Fatal(err)
	}
	dst, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := dst.DB.Exec(`CREATE TABLE vecs (id TEXT PRIMARY KEY, n INTEGER, f REAL, v BLOB)`); err != nil {
		t.Fatal(err)
	}
	if err := dst.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ImportLogical(ctx, raw); err != nil {
		t.Fatal(err)
	}
	cols, _ := src.columns("vecs")
	a, _ := src.Checksum(ctx, "vecs", cols)
	b, _ := dst.Checksum(ctx, "vecs", cols)
	if a != b {
		t.Fatalf("checksum mismatch after dump round trip: %+v vs %+v", a, b)
	}
	var got []byte
	_ = dst.DB.QueryRow(`SELECT v FROM vecs WHERE id='x'`).Scan(&got)
	if string(got) != string(blob) {
		t.Fatalf("blob changed: %v", got)
	}
}

// Backups written before the dump was streamed (one indented JSON document) restore unchanged, and a
// streamed dump of many rows restores every row.
func TestLogicalDumpStreamsAndReadsOlderDumps(t *testing.T) {
	ctx := context.Background()
	older := `{
  "backend": "tidb",
  "format": 2,
  "tables": {
    "traces": [
      {
        "body": "{}",
        "created_at": "2026-09-23T00:00:00Z",
        "id": "t1"
      }
    ],
    "vecs": [
      {
        "id": "x",
        "v": {
          "b64": "AP+AEMM="
        }
      }
    ]
  }
}`
	dst, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := dst.DB.Exec(`CREATE TABLE vecs (id TEXT PRIMARY KEY, v BLOB)`); err != nil {
		t.Fatal(err)
	}
	n, err := dst.ImportLogical(ctx, []byte(older))
	if err != nil || n != 2 {
		t.Fatalf("older dump: %d rows, %v", n, err)
	}
	var got []byte
	if err := dst.DB.QueryRow(`SELECT v FROM vecs WHERE id='x'`).Scan(&got); err != nil || string(got) != "\x00\xff\x80\x10\xc3" {
		t.Fatalf("blob %v %v", got, err)
	}

	src, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	tx, _ := src.DB.Begin()
	for i := 0; i < 5000; i++ {
		if _, err := tx.Exec(`INSERT INTO traces(id, body, created_at) VALUES(?,?,?)`, fmt.Sprintf("t%05d", i), `{"a":"철수 🎉"}`, "2026-09-23T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	_ = tx.Commit()
	var buf bytes.Buffer
	if err := src.ExportLogicalTo(&buf); err != nil {
		t.Fatal(err)
	}
	fresh, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := fresh.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := fresh.ImportLogicalFrom(ctx, &buf); err != nil || n < 5000 {
		t.Fatalf("streamed dump: %d rows, %v", n, err)
	}
	cols, _ := src.columns("traces")
	a, _ := src.Checksum(ctx, "traces", cols)
	b, _ := fresh.Checksum(ctx, "traces", cols)
	if a != b {
		t.Fatalf("checksum mismatch: %+v vs %+v", a, b)
	}
}
