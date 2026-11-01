package application

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/providers"
)

// burstReader fails a Read that asks for more than max bytes. io.ReadAll grows its
// buffer past that; a fixed copy buffer does not.
type burstReader struct {
	left int
	max  int
}

func (b *burstReader) Read(p []byte) (int, error) {
	if len(p) > b.max {
		return 0, fmt.Errorf("read of %d bytes exceeds the %d byte buffer", len(p), b.max)
	}
	if b.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.left)
	for i := range n {
		p[i] = byte(i)
	}
	b.left -= n
	return n, nil
}

func TestUpdateEntryCopyDoesNotBufferTheWholeFile(t *testing.T) {
	const size = 1 << 20
	var buf bytes.Buffer
	n, err := writeUpdateEntry("masamong", &burstReader{left: size, max: 64 << 10}, uint64(size), &buf, size)
	if err != nil || n != size || buf.Len() != size {
		t.Fatalf("streamed entry = %d, %v", n, err)
	}
}

type capFile struct {
	*os.File
	max int
}

func (f *capFile) Read(p []byte) (int, error) {
	if len(p) > f.max {
		return 0, fmt.Errorf("read of %d bytes exceeds %d", len(p), f.max)
	}
	return f.File.Read(p)
}

func (f *capFile) ReadAt(p []byte, off int64) (int, error) {
	if len(p) > f.max {
		return 0, fmt.Errorf("read of %d bytes exceeds %d", len(p), f.max)
	}
	return f.File.ReadAt(p, off)
}

func writeSizedArchive(t *testing.T, path, format string, files map[string]int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chunk := make([]byte, 32<<10)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	writeN := func(w io.Writer, n int) {
		t.Helper()
		for n > 0 {
			c := min(len(chunk), n)
			if _, err := w.Write(chunk[:c]); err != nil {
				t.Fatal(err)
			}
			n -= c
		}
	}
	switch format {
	case "zip":
		zw := zip.NewWriter(f)
		for name, n := range files {
			w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
			if err != nil {
				t.Fatal(err)
			}
			writeN(w, n)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		for name, n := range files {
			if err := tw.WriteHeader(&tar.Header{Name: "r/" + name, Mode: 0o755, Size: int64(n), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			writeN(tw, n)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func withCappedArchiveOpen(t *testing.T, max int) {
	t.Helper()
	prev := openUpdateFile
	openUpdateFile = func(name string) (archiveFile, error) {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		return &capFile{File: f, max: max}, nil
	}
	t.Cleanup(func() { openUpdateFile = prev })
}

func programFiles(n int) map[string]int {
	return map[string]int{exeName("masamong"): n, exeName("masamongd"): n}
}

func allocOf(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func extractSized(t *testing.T, format string, n int) {
	t.Helper()
	dir := t.TempDir()
	name := "rel." + format
	arc := filepath.Join(dir, name)
	writeSizedArchive(t, arc, format, programFiles(n))
	staged, err := extractBinariesTo(dir, arc, name)
	if err != nil {
		t.Fatal(err)
	}
	for base, size := range programFiles(n) {
		st, err := os.Stat(staged[base])
		if err != nil || st.Size() != int64(size) {
			t.Fatalf("%s: %v size %v", base, err, st)
		}
	}
	removeStaged(staged)
}

// Extraction reads the archive in small pieces. A 1 MiB program must not allocate
// like a 1 MiB program, and no read may ask for the whole file.
func TestUpdateExtractDoesNotBufferTheBinary(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			withCappedArchiveOpen(t, 256<<10)
			extractSized(t, format, 32<<10) // warm the decoder
			var small, large uint64
			for range 2 {
				if d := allocOf(func() { extractSized(t, format, 64<<10) }); small == 0 || d < small {
					small = d
				}
				if d := allocOf(func() { extractSized(t, format, 1<<20) }); large == 0 || d < large {
					large = d
				}
			}
			if large > small+512<<10 {
				t.Fatalf("alloc grew by %d (64KiB=%d, 1MiB=%d); extraction buffered the binary", large-small, small, large)
			}
		})
	}
}

// A release may ship data next to the two programs. That data is never written,
// so a declared size over the archive cap must not fail the update. A second
// copy of a program is skipped the same way, and the first copy is the one staged.
func TestUpdateExtractSkipsOversizedUnwantedEntries(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			name := "rel." + format
			arc := filepath.Join(dir, name)
			writeArchiveWithOversizedExtra(t, arc, format)
			staged, err := extractBinariesTo(dir, arc, name)
			if err != nil {
				t.Fatal(err)
			}
			defer removeStaged(staged)
			cli, err := os.ReadFile(staged[exeName("masamong")])
			if err != nil || string(cli) != "program-masamong" {
				t.Fatalf("masamong: %q %v", cli, err)
			}
			daemon, err := os.ReadFile(staged[exeName("masamongd")])
			if err != nil || string(daemon) != "program-masamongd" {
				t.Fatalf("masamongd: %q %v", daemon, err)
			}
		})
	}
}

func writeArchiveWithOversizedExtra(t *testing.T, path, format string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	cli := exeName("masamong")
	daemon := exeName("masamongd")
	switch format {
	case "zip":
		zw := zip.NewWriter(f)
		writeZip := func(name, body string) {
			t.Helper()
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(w, body); err != nil {
				t.Fatal(err)
			}
		}
		writeZip("share/weights.bin", "not-a-program")
		writeZip(cli, "program-masamong")
		writeZip(daemon, "program-masamongd")
		writeZip("bin/"+cli, "duplicate-masamong")
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		tw := tar.NewWriter(gz)
		writeTar := func(name, body string) {
			t.Helper()
			if err := tw.WriteHeader(&tar.Header{Name: "bin/" + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(tw, body); err != nil {
				t.Fatal(err)
			}
		}
		// The extra file's header says it is over the cap. The body is zeros, so the
		// gzip on disk stays small; skipping it still has to walk that many bytes.
		huge := int64(updateArchiveMax) + 1
		if err := tw.WriteHeader(&tar.Header{Name: "share/weights.bin", Mode: 0o644, Size: huge, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 1<<20)
		for huge > 0 {
			n := int64(len(buf))
			if n > huge {
				n = huge
			}
			if _, err := tw.Write(buf[:n]); err != nil {
				t.Fatal(err)
			}
			huge -= n
		}
		writeTar(cli, "program-masamong")
		writeTar(daemon, "program-masamongd")
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if format == "zip" {
		// The zip writer records the bytes it was given. Patch the declared size
		// of the entries we skip so the cap is what the header claims, not the payload.
		patchZipUncompressed(t, path, "share/weights.bin", 0, uint32(updateArchiveMax)+1)
		patchZipUncompressed(t, path, "bin/"+cli, 0, uint32(updateArchiveMax)+1)
	}
}

func patchZipUncompressed(t *testing.T, path, name string, nth int, size uint32) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sig := []byte{0x50, 0x4b, 0x01, 0x02}
	want := []byte(name)
	seen := 0
	for i := 0; i+46 <= len(b); i++ {
		if !bytes.Equal(b[i:i+4], sig) {
			continue
		}
		nlen := int(binary.LittleEndian.Uint16(b[i+28 : i+30]))
		if nlen < len(want) || i+46+nlen > len(b) || !bytes.Equal(b[i+46:i+46+nlen], want) {
			continue
		}
		if seen != nth {
			seen++
			continue
		}
		binary.LittleEndian.PutUint32(b[i+24:i+28], size)
		local := int(binary.LittleEndian.Uint32(b[i+42 : i+46]))
		if local >= 0 && local+30 <= len(b) && bytes.Equal(b[local:local+4], []byte{0x50, 0x4b, 0x03, 0x04}) {
			binary.LittleEndian.PutUint32(b[local+22:local+26], size)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("zip entry %s #%d not found", name, nth)
}

func TestUpdateExtractFailureRemovesStagedFiles(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			name := "rel." + format
			arc := filepath.Join(dir, name)
			writeSizedArchive(t, arc, format, map[string]int{exeName("masamong"): 20})
			_, err := extractBinariesTo(dir, arc, name)
			if err == nil || !strings.Contains(err.Error(), "has no") {
				t.Fatalf("missing program: %v", err)
			}
			if left := updateTempFiles(t, dir); len(left) != 0 {
				t.Fatalf("missing program left %v", left)
			}

			writeSizedArchive(t, arc, format, programFiles(40))
			_, err = extractBinariesLimited(dir, arc, name, 50)
			if err == nil || !strings.Contains(err.Error(), "too large") {
				t.Fatalf("combined cap: %v", err)
			}
			if left := updateTempFiles(t, dir); len(left) != 0 {
				t.Fatalf("over-cap extract left %v", left)
			}
		})
	}
}

func TestUpdateDownloadFailureRemovesTheTempFile(t *testing.T) {
	dir := t.TempDir()
	body := []byte("archive-bytes")
	sum := sha256.Sum256(body)
	name := "masamong_test.tar.gz"
	var good atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			hash := strings.Repeat("ab", 32)
			if good.Load() {
				hash = strings.ToUpper(hex.EncodeToString(sum[:]))
			}
			fmt.Fprintf(w, "%s  %s\n", hash, name)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	old := updateDownloadPrefix
	t.Cleanup(func() { updateDownloadPrefix = old })
	updateDownloadPrefix = srv.URL + "/dl/%s/"
	base := srv.URL + "/dl/" + UpdateRepo + "/v1/"
	st := updateState{Asset: &updateAsset{Name: name, URL: base + name}, SumsURL: base + "SHA256SUMS"}
	if _, err := downloadRelease(context.Background(), &Service{Log: slog.Default()}, st, dir); err == nil || !strings.Contains(err.Error(), "does not match SHA256SUMS") {
		t.Fatalf("bad sum: %v", err)
	}
	if left := updateTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("bad sum left %v", left)
	}

	// the same bytes with the real sum (any case) land in one file
	good.Store(true)
	arc, err := downloadRelease(context.Background(), &Service{Log: slog.Default()}, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(arc)
	_ = os.Remove(arc)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("streamed archive = %q, %v", got, err)
	}
}

func TestUpdateDownloadOverCapAndCancelRemoveTheTempFile(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64))
	}))
	defer srv.Close()
	c := providers.NewDownloadClient(time.Minute, slog.Default(), func(string) bool { return true })
	c.MaxRetries = 0
	if _, _, err := streamReleaseAsset(context.Background(), c, srv.URL, dir, "a.tar.gz", 8); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("over cap: %v", err)
	}
	if left := updateTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("over cap left %v", left)
	}

	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer blocked.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, _, err := streamReleaseAsset(ctx, c, blocked.URL, dir, "a.tar.gz", 1<<20)
	<-done
	if err == nil {
		t.Fatal("cancelled download succeeded")
	}
	if left := updateTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("cancelled download left %v", left)
	}
}

func TestUpdateDownloadRetriesThenKeepsOneFile(t *testing.T) {
	var hits atomic.Int32
	body := []byte("second-try")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "later", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	c := providers.NewDownloadClient(time.Minute, slog.Default(), func(string) bool { return true })
	c.MaxRetries = 1
	c.RetryBase = time.Millisecond
	dir := t.TempDir()
	path, sum, err := streamReleaseAsset(context.Background(), c, srv.URL, dir, "a.tar.gz", 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, body) || hex.EncodeToString(sum) != hex.EncodeToString(sha256Sum(body)) {
		t.Fatalf("retry got %q sum %x", got, sum)
	}
	if left := updateTempFiles(t, dir); len(left) != 1 {
		t.Fatalf("retry left %v", left)
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func TestUpdateArchiveDownloadRefusesTheNetworkWhenOffline(t *testing.T) {
	c := providers.NewDownloadClient(time.Second, slog.Default(), func(string) bool { return true })
	c.MaxRetries = 0
	_, _, err := streamReleaseAsset(context.Background(), c, "https://example.com/masamong.tar.gz", t.TempDir(), "masamong.tar.gz", 1024)
	if !errors.Is(err, providers.ErrOffline) {
		t.Fatalf("offline: %v", err)
	}
}

// A crash can leave the archive temp file in the install directory. Both cleanups
// remove .masamong-dl-* and must not touch the installed programs.
func TestUpdateDownloadLeftoversAreRemoved(t *testing.T) {
	dir, _ := updateFixture(t)
	cli := filepath.Join(dir, exeName("masamong"))
	daemon := filepath.Join(dir, exeName("masamongd"))
	cliBefore, err := os.ReadFile(cli)
	if err != nil {
		t.Fatal(err)
	}
	daemonBefore, err := os.ReadFile(daemon)
	if err != nil {
		t.Fatal(err)
	}
	programs := func() {
		t.Helper()
		got, err := os.ReadFile(cli)
		if err != nil || !bytes.Equal(got, cliBefore) {
			t.Fatalf("masamong = %q, %v", got, err)
		}
		got, err = os.ReadFile(daemon)
		if err != nil || !bytes.Equal(got, daemonBefore) {
			t.Fatalf("masamongd = %q, %v", got, err)
		}
	}
	leftover := filepath.Join(dir, ".masamong-dl-crash")
	if err := os.WriteFile(leftover, []byte("partial archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CleanUpdateLeftovers(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
		t.Fatalf("CleanUpdateLeftovers left the download: %v", err)
	}
	programs()

	if err := os.WriteFile(leftover, []byte("stale archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer srv.Close()
	old := updateDownloadPrefix
	t.Cleanup(func() { updateDownloadPrefix = old })
	updateDownloadPrefix = srv.URL + "/dl/%s/"
	base := srv.URL + "/dl/" + UpdateRepo + "/v1/"
	name := "masamong_test.tar.gz"
	st := updateState{Asset: &updateAsset{Name: name, URL: base + name}, SumsURL: base + "SHA256SUMS"}
	if _, err := downloadRelease(context.Background(), &Service{Log: slog.Default()}, st, dir); err == nil {
		t.Fatal("a failing release download succeeded")
	}
	if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
		t.Fatalf("download start left the stale archive: %v", err)
	}
	programs()
	if left := updateTempFiles(t, dir); len(left) != 0 {
		t.Fatalf("download left %v", left)
	}
}
