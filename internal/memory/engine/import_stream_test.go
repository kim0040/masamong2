package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

type cancelAfterChecks struct {
	context.Context
	cancel context.CancelFunc
	n      atomic.Int64
}

func (c *cancelAfterChecks) Err() error {
	if c.n.Add(1) >= 700 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestImportBatchesCancelAndResume(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	p := filepath.Join(t.TempDir(), "rows.csv")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	_, _ = fmt.Fprintln(w, "Date,User,Message")
	for i := 0; i < 4000; i++ {
		_, _ = fmt.Fprintf(w, "2026-01-01 10:00:00,Alice,Message %d\n", i)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelAfterChecks{Context: parent, cancel: cancel}
	partial, err := e.ImportKakaoCSVContext(ctx, p, "test", "kakao:test", "Test", "UTC", corpus.DefaultMarkerLocale)
	if !errors.Is(err, context.Canceled) || partial.Inserted == 0 || partial.Inserted >= 4000 || partial.Inserted%256 != 0 {
		t.Fatalf("partial result: %+v %v", partial, err)
	}
	res, err := e.ImportKakaoCSVContext(context.Background(), p, "test", "kakao:test", "Test", "UTC", corpus.DefaultMarkerLocale)
	if err != nil || res.Duplicate != partial.Inserted || res.Inserted+partial.Inserted != 4000 {
		t.Fatalf("resume: %+v partial=%+v err=%v", res, partial, err)
	}
	var n int
	_ = e.Store.DB.DB.QueryRow(`SELECT COUNT(*) FROM memory_observations`).Scan(&n)
	if n != 4000 {
		t.Fatal(n)
	}
}

func TestImportGeneratedFileHeapBounded(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	p := filepath.Join(t.TempDir(), "large.csv")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	_, _ = fmt.Fprintln(w, "Date,User,Message")
	for i := 0; i < 24000; i++ {
		_, _ = fmt.Fprintln(w, "2026-01-01 10:00:00,Alice,"+strings.Repeat("x", 1024))
	}
	_ = w.Flush()
	_ = f.Close()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stop := make(chan struct{})
	peakDone := make(chan uint64, 1)
	go func() {
		peak := before.HeapAlloc
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var st runtime.MemStats
			runtime.ReadMemStats(&st)
			if st.HeapAlloc > peak {
				peak = st.HeapAlloc
			}
			select {
			case <-stop:
				peakDone <- peak
				return
			case <-ticker.C:
			}
		}
	}()
	res, err := e.ImportKakaoCSVContext(context.Background(), p, "test", "kakao:test", "Test", "UTC", corpus.DefaultMarkerLocale)
	close(stop)
	peak := <-peakDone
	if err != nil || res.Inserted != 24000 {
		t.Fatalf("import: %+v %v", res, err)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > before.HeapAlloc+(16<<20) {
		t.Fatalf("heap growth: %d", after.HeapAlloc-before.HeapAlloc)
	}
	if peak > before.HeapAlloc+(24<<20) {
		t.Fatalf("peak heap grew with the corpus: before=%d peak=%d", before.HeapAlloc, peak)
	}
	t.Logf("24 MiB import: peak heap growth %d bytes", int64(peak)-int64(before.HeapAlloc))
	// Imports and parser share the supported byte limit. 1 GiB is acceptable now that parsing streams.
	if corpus.MaxImportBytes != 1<<30 {
		t.Fatal(corpus.MaxImportBytes)
	}
}
