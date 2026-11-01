package corpus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeCSV(t *testing.T, rows, fieldBytes int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "large.csv")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	_, _ = fmt.Fprintln(w, "Date,User,Message")
	field := strings.Repeat("x", fieldBytes)
	for i := 0; i < rows; i++ {
		if _, err := fmt.Fprintln(w, "2026-01-01 10:00:00,Alice,"+field); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKakaoStreamingCancellation(t *testing.T) {
	p := writeCSV(t, 1000, 128)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	_, n, err := ReadKakaoCSVContext(ctx, p, "test", "kakao:test", DefaultMarkerLocale, func(o Observation) error {
		seen++
		if seen == 100 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || seen != 100 || n != 100 {
		t.Fatalf("cancellation: seen=%d n=%d err=%v", seen, n, err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, _, err := ReadKakaoCSVContext(cancelled, p, "test", "kakao:test", DefaultMarkerLocale, func(Observation) error { t.Fatal("callback after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestKakaoRejectsHugeSingleFieldBeforeAccumulatingIt(t *testing.T) {
	for _, size := range []int{MaxFieldBytes + 1, 4 << 20} {
		p := writeCSV(t, 1, size)
		if _, _, err := ReadKakaoCSV(p, "test", "kakao:test", func(Observation) error { t.Fatal("oversized record delivered"); return nil }); !errors.Is(err, ErrFieldTooLarge) && !errors.Is(err, ErrRowTooLarge) {
			t.Fatalf("%d: %v", size, err)
		}
	}
}

func TestKakaoStreamingHeapStaysBounded(t *testing.T) {
	p := writeCSV(t, 32768, 1024) // over 32 MiB; parsing retains one row, never the file/corpus
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	peak := before.HeapAlloc
	seen := 0
	_, n, err := ReadKakaoCSV(p, "test", "kakao:test", func(Observation) error {
		seen++
		if seen%4096 == 0 {
			runtime.GC()
			var st runtime.MemStats
			runtime.ReadMemStats(&st)
			if st.HeapAlloc > peak {
				peak = st.HeapAlloc
			}
		}
		return nil
	})
	if err != nil || n != 32768 {
		t.Fatalf("read: %d %v", n, err)
	}
	if peak > before.HeapAlloc+(12<<20) {
		t.Fatalf("retained heap grew with file: before=%d peak=%d", before.HeapAlloc, peak)
	}
	t.Logf("32 MiB stream: retained heap growth %d bytes", int64(peak)-int64(before.HeapAlloc))
}

func TestKakaoBOMCRLFAndQuotedMultilineRemainSupported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bom.csv")
	if err := os.WriteFile(p, []byte("\xef\xbb\xbfDate,User,Message\r\n2026-01-01 10:00:00,Alice,\"first\r\nsecond\"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var content string
	if _, n, err := ReadKakaoCSV(p, "test", "kakao:test", func(o Observation) error { content = o.Content; return nil }); err != nil || n != 1 || content != "first\nsecond" {
		t.Fatalf("%q %d %v", content, n, err)
	}
}
