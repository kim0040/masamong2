package application

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/fsutil"
	"masamong/internal/launcher"
	"masamong/internal/providers"
)

// In-app update (updates.apply): the console installs the release the daily check found, the same way
// install.sh updates an installation. It downloads this computer's archive and SHA256SUMS from the
// GitHub release, checks the archive against the sum, takes masamong and masamongd out of it, makes
// sure the new masamong runs here and reports the expected version, backs the instance up, puts the two
// programs in place of the running ones (a rename, so a failure leaves the old ones) and restarts the
// service. The data folder is never touched by the update itself.
//
// It only updates release builds installed as two programs side by side (install.sh, install.ps1, a
// release archive). A source build (a version without x.y.z) and a folder the service cannot write are
// told to use the installer instead.

// updateDownloadPrefix is where release files may come from; tests point it at a local server.
var updateDownloadPrefix = "https://github.com/%s/releases/download/"

// updateRedirectHost says which hosts a release download may be redirected to: GitHub answers a
// release asset with a redirect to its file host. Tests widen it to their local server.
var updateRedirectHost = func(host string) bool {
	return host == "github.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

// selfExecutable is the running program's path; tests point it at a temporary folder.
var selfExecutable = os.Executable

const (
	updateArchiveMax = 256 << 20
	updateSumsMax    = 1 << 20
	// updateExtractMax is the combined uncompressed size of the two programs.
	// Each one used to be allowed updateArchiveMax on its own, and both were
	// held in memory with the archive, so a release could ask for ~768 MiB
	// on a 512 MB machine. The archive now stays on disk; this caps what is
	// written out of it.
	updateExtractMax = 256 << 20
	updateCopyBuf    = 32 << 10
)

// updateAsset is this computer's archive in a release.
type updateAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size,omitempty"`
}

// releaseArch is the archive's CPU name (release.sh): arm is shipped as armv7.
func releaseArch() string {
	if runtime.GOARCH == "arm" {
		return "armv7"
	}
	return runtime.GOARCH
}

// updateAssetName is this computer's archive name: masamong_<os>_<arch>.tar.gz (.zip on Windows).
func updateAssetName() string {
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return "masamong_" + runtime.GOOS + "_" + releaseArch() + ext
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// releaseVersion reports whether v is a release version (1.2.3 or v1.2.3-rc1), not a source build.
func releaseVersion(v string) bool {
	return !sourceBuild(v) && (newerVersion(v, "0.0.0") || v == "0.0.0")
}

// InstallDir is the folder the running programs really live in, links resolved: os.Executable is not
// resolved on every platform, so an install reached through a link (/usr/local/bin/masamongd ->
// /opt/masamong/masamongd) would otherwise look for its program-update journal in the wrong folder.
// Every place that reads or writes that journal or the old/new program copies uses this one answer.
func InstallDir() (string, error) {
	self, err := selfExecutable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	return filepath.Dir(self), nil
}

// updateDir is the folder of the running programs when it can take an update, or why it cannot.
func updateDir() (string, string) {
	if devBuild(Version) || !releaseVersion(Version) {
		return "", "source_build"
	}
	dir, err := InstallDir()
	if err != nil {
		return "", "unknown_location"
	}
	for _, b := range []string{"masamong", "masamongd"} {
		if st, err := os.Stat(filepath.Join(dir, exeName(b))); err != nil || !st.Mode().IsRegular() {
			return "", "not_side_by_side"
		}
	}
	probe, err := os.CreateTemp(dir, ".masamong-write-*")
	if err != nil {
		return "", "not_writable"
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	return dir, ""
}

// updateBlocked is why updates.apply cannot install the newest release now ("" = it can).
func (s *Service) updateBlocked(st updateState) string {
	switch {
	case s.practice:
		return "practice"
	case st.Status != "ok" || !newerVersion(st.Latest, Version):
		return "no_update"
	case st.Asset == nil || st.SumsURL == "":
		return "no_asset"
	}
	_, why := updateDir()
	return why
}

func (s *Service) updateApply(ctx context.Context, req action.Request) action.Result {
	fail := func(code string, msg string, details map[string]any) action.Result {
		return action.Fail(req, code, "generic.error", msg, details)
	}
	if !s.Daemon {
		return fail(errcode.InvalidInput, "the service does not run in this process: update from the console or with install.sh", nil)
	}
	unlock, err := lockMaintenance(s.DataRoot, false)
	if err != nil {
		return fail(errcode.IO, err.Error(), nil)
	}
	defer unlock()
	if !updateApplying.CompareAndSwap(false, true) {
		return fail(errcode.InvalidInput, "an update is already being installed", nil)
	}
	defer updateApplying.Store(false)
	var st updateState
	_, _ = s.kvGet("updates:state", &st)
	if why := s.updateBlocked(st); why != "" {
		return fail(errcode.InvalidInput, "this installation cannot update itself now: "+why, map[string]any{"reason": why})
	}
	if strVal(inputMap(req), "confirm", "") != st.Latest {
		return fail(errcode.InvalidInput, "confirm must repeat the version to install", map[string]any{"version": st.Latest})
	}
	dir, _ := updateDir()
	installLock, err := launcher.AcquireNamedLock(dir, ".masamong-update.lock")
	if err != nil {
		return fail(errcode.IO, err.Error(), nil)
	}
	defer installLock.Release()
	if _, err := os.Lstat(filepath.Join(dir, updatePending)); !os.IsNotExist(err) {
		return fail(errcode.IO, "program maintenance recovery required", nil)
	}
	arcPath, err := downloadRelease(ctx, s, st, dir)
	if err != nil {
		return fail(errcode.ProviderUnavailable, "download failed, nothing was changed: "+err.Error(), nil)
	}
	// The archive is only needed until the programs are on disk. Drop it before the
	// database backup, which needs the free space the download just used.
	defer os.Remove(arcPath)
	staged, err := stageBinariesFromArchive(ctx, dir, arcPath, st.Asset.Name, st.Latest)
	if err != nil {
		return fail(errcode.IO, "the new version could not be prepared, nothing was changed: "+err.Error(), nil)
	}
	_ = os.Remove(arcPath)
	backup, _, err := s.createBackupLocked(backupPreUpdate)
	if err != nil {
		removeStaged(staged)
		return fail(errcode.IO, "backup failed, nothing was changed: "+err.Error(), nil)
	}
	s.pruneKindLocked(backupPreUpdate, updateBackupsKept)
	if err := swapBinariesLocked(dir, staged); err != nil {
		if _, pending := os.Lstat(filepath.Join(dir, updatePending)); os.IsNotExist(pending) {
			removeStaged(staged)
		}
		return fail(errcode.IO, "installing failed; maintenance recovery may be required: "+err.Error(), map[string]any{"backup": backup})
	}
	_ = s.kvSet("updates:applied", map[string]any{"from": Version, "to": st.Latest, "at": s.Clock.Now().UTC().Format(time.RFC3339), "backup": backup})
	s.Log.Info("update installed; restarting", "from", Version, "to", st.Latest, "backup", backup)
	s.requestLifecycle(lifecycleRestart)
	return action.OK(req, map[string]any{"installed": st.Latest, "from": Version, "backup": backup, "restarting": true, "message": s.msg(req, "updates.installed", "version", st.Latest)})
}

// downloadRelease fetches SHA256SUMS, streams the archive to a temp file in dir while hashing it,
// and returns that file. The caller deletes it. The sum check is the same one as before: the hex
// digest of every byte, lower-cased, must equal the SHA256SUMS line for this asset.
func downloadRelease(ctx context.Context, s *Service, st updateState, dir string) (string, error) {
	prefix := strings.Replace(updateDownloadPrefix, "%s", UpdateRepo, 1)
	for _, u := range []string{st.Asset.URL, st.SumsURL} {
		if !strings.HasPrefix(u, prefix) {
			return "", fmt.Errorf("unexpected download address %s", u)
		}
	}
	// A previous attempt may have died after writing the archive and before deleting it.
	if stale, _ := filepath.Glob(filepath.Join(dir, ".masamong-dl-*")); len(stale) > 0 {
		for _, p := range stale {
			_ = os.Remove(p)
		}
	}
	client := providers.NewDownloadClient(10*time.Minute, s.Log, updateRedirectHost)
	client.UserAgent = "masamong/" + Version
	client.MaxRetries = 1
	client.MaxBody = updateSumsMax
	sums, err := client.Get(ctx, st.SumsURL, map[string]string{"Accept": "application/octet-stream"})
	if err != nil {
		return "", fmt.Errorf("SHA256SUMS: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums.Body), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == st.Asset.Name {
			want = strings.ToLower(f[0])
		}
	}
	if len(want) != 64 {
		return "", fmt.Errorf("SHA256SUMS has no line for %s", st.Asset.Name)
	}
	path, sum, err := streamReleaseAsset(ctx, client, st.Asset.URL, dir, st.Asset.Name, updateArchiveMax)
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(sum) != want {
		_ = os.Remove(path)
		return "", fmt.Errorf("%s does not match SHA256SUMS", st.Asset.Name)
	}
	return path, nil
}

// streamReleaseAsset writes the body to a new file in dir and returns its SHA-256.
// client.Get would keep the whole archive in memory, so this uses the download
// client's HTTP client (redirect policy, timeout) and copies the body itself.
// One retry matches MaxRetries = 1. A failed attempt deletes its file.
func streamReleaseAsset(ctx context.Context, client *providers.Client, rawURL, dir, assetName string, max int64) (string, []byte, error) {
	attempts := client.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		path, sum, retry, err := streamReleaseOnce(ctx, client, rawURL, dir, assetName, max)
		if err == nil {
			return path, sum, nil
		}
		if path != "" {
			_ = os.Remove(path)
		}
		last = err
		if !retry || attempt == attempts {
			break
		}
		if client.RetryBase > 0 {
			timer := time.NewTimer(client.RetryBase)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return "", nil, last
}

func streamReleaseOnce(ctx context.Context, client *providers.Client, rawURL, dir, assetName string, max int64) (path string, sum []byte, retry bool, err error) {
	if releaseOfflineBlocked(rawURL) {
		return "", nil, false, fmt.Errorf("%s: %w", assetName, providers.ErrOffline)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, false, fmt.Errorf("%s: %w", assetName, err)
	}
	req.Header.Set("User-Agent", client.UserAgent)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return "", nil, streamRetryable(err), fmt.Errorf("%s: %w", assetName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return "", nil, retry, fmt.Errorf("%s: status %d", assetName, resp.StatusCode)
	}
	if resp.ContentLength > max {
		return "", nil, false, fmt.Errorf("%s: body exceeds %d bytes", assetName, max)
	}
	f, err := os.CreateTemp(dir, ".masamong-dl-*")
	if err != nil {
		return "", nil, false, err
	}
	path = f.Name()
	h := sha256.New()
	buf := make([]byte, updateCopyBuf)
	n, copyErr := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(resp.Body, max+1), buf)
	// No Sync. The archive is hashed and deleted once the programs are staged;
	// syncing it would stall a slow disk for the whole file, and a crash just
	// downloads it again. The staged programs are still synced.
	if cerr := f.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		return path, nil, streamRetryable(copyErr), fmt.Errorf("%s: %w", assetName, copyErr)
	}
	if n > max {
		return path, nil, false, fmt.Errorf("%s: body exceeds %d bytes", assetName, max)
	}
	return path, h.Sum(nil), false, nil
}

// releaseOfflineBlocked is the download client's offline rule. The streaming path
// cannot call Client.Get, which is what normally enforces it, and still must not
// dial out while MASAMONG_OFFLINE=1. Loopback stays allowed for tests.
func releaseOfflineBlocked(rawURL string) bool {
	if os.Getenv("MASAMONG_OFFLINE") != "1" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	h := u.Hostname()
	if h == "localhost" {
		return false
	}
	ip := net.ParseIP(h)
	return ip == nil || !ip.IsLoopback()
}

func streamRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, providers.ErrCrossHostRedirect) {
		return false
	}
	return true
}

// archiveFile is the release on disk. Zip reads it by offset; tar.gz reads it once.
// Tests wrap the open so a read that asks for the whole file fails.
type archiveFile interface {
	io.Reader
	io.ReaderAt
	io.Seeker
	io.Closer
	Stat() (os.FileInfo, error)
}

var openUpdateFile = func(name string) (archiveFile, error) {
	return os.Open(name)
}

// extractBinariesTo writes masamong and masamongd straight into staged files in dir.
func extractBinariesTo(dir, archivePath, name string) (map[string]string, error) {
	return extractBinariesLimited(dir, archivePath, name, updateExtractMax)
}

// extractBinariesLimited is extractBinariesTo with an explicit combined cap.
func extractBinariesLimited(dir, archivePath, name string, combined int64) (staged map[string]string, err error) {
	f, err := openUpdateFile(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	want := map[string]bool{exeName("masamong"): true, exeName("masamongd"): true}
	staged = map[string]string{}
	// files keeps the map if a `return nil, err` clears the named result.
	// Every failure path has to delete staged programs, including that one.
	files := staged
	defer func() {
		if err != nil {
			removeStaged(files)
		}
	}()
	left := combined
	take := func(entry string, r io.Reader, size uint64) error {
		base := path.Base(strings.ReplaceAll(entry, `\`, "/"))
		// Entries we do not write are skipped with no size check. A release can
		// carry data beside the two programs; that data is never copied, so its
		// declared size must not fail the update. The cap still applies below,
		// to the programs that are actually staged.
		if !want[base] || staged[base] != "" {
			return nil
		}
		out, err := os.CreateTemp(dir, "."+base+".new-*")
		if err != nil {
			return err
		}
		staged[base] = out.Name()
		// The per-file cap and whatever remains of the combined cap, whichever is smaller.
		max := int64(updateArchiveMax)
		if left < max {
			max = left
		}
		n, err := writeUpdateEntry(base, r, size, out, max)
		if err == nil {
			err = out.Chmod(0o755)
		}
		if err == nil {
			err = out.Sync()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(out.Name(), 0o755)
		}
		if err != nil {
			return err
		}
		left -= n
		return nil
	}
	if strings.HasSuffix(name, ".zip") {
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(f, st.Size())
		if err != nil {
			return nil, err
		}
		for _, zf := range zr.File {
			if !zf.Mode().IsRegular() {
				continue
			}
			base := path.Base(strings.ReplaceAll(zf.Name, `\`, "/"))
			// A declared size over the cap is refused before the inflater runs.
			// Only entries we are about to write: a skipped file is not inflated.
			if want[base] && staged[base] == "" && (zf.UncompressedSize64 > uint64(updateArchiveMax) || int64(zf.UncompressedSize64) > left) {
				return nil, fmt.Errorf("%s is too large", base)
			}
			if !want[base] || staged[base] != "" {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return nil, err
			}
			err = take(zf.Name, rc, zf.UncompressedSize64)
			if closeErr := rc.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return nil, err
			}
		}
	} else {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Typeflag != tar.TypeReg {
				continue
			}
			if h.Size < 0 {
				return nil, fmt.Errorf("%s has an invalid size", h.Name)
			}
			if err := take(h.Name, tr, uint64(h.Size)); err != nil {
				return nil, err
			}
		}
	}
	for b := range want {
		if staged[b] == "" {
			return nil, fmt.Errorf("the archive has no %s", b)
		}
	}
	return staged, nil
}

// writeUpdateEntry copies one archive entry to w. declared and the bytes actually
// read are both capped at max; the copy buffer stays updateCopyBuf, so the entry
// is never held whole.
func writeUpdateEntry(name string, r io.Reader, declared uint64, w io.Writer, max int64) (int64, error) {
	if max < 0 || declared > uint64(max) {
		return 0, fmt.Errorf("%s is too large", name)
	}
	buf := make([]byte, updateCopyBuf)
	// Hide ReadFrom. *os.File and bytes.Buffer would otherwise copy with their own
	// buffer, and bytes.Buffer grows that buffer to the size of the entry.
	n, err := io.CopyBuffer(struct{ io.Writer }{w}, io.LimitReader(r, max+1), buf)
	if err != nil {
		return n, err
	}
	if n > max {
		return n, fmt.Errorf("%s is too large", name)
	}
	return n, nil
}

// stageBinariesFromArchive extracts the two programs and checks they run here as version.
func stageBinariesFromArchive(ctx context.Context, dir, archivePath, archiveName, version string) (map[string]string, error) {
	staged, err := extractBinariesTo(dir, archivePath, archiveName)
	if err != nil {
		return nil, err
	}
	if err := verifyStaged(ctx, staged, version); err != nil {
		return nil, err
	}
	return staged, nil
}

// stageBinaries writes programs that are already in memory. The update path does not use it;
// maintenance tests stage a copy of the test binary this way.
func stageBinaries(ctx context.Context, dir string, files map[string][]byte, version string) (map[string]string, error) {
	staged := map[string]string{}
	for name, b := range files {
		f, err := os.CreateTemp(dir, "."+name+".new-*")
		if err != nil {
			removeStaged(staged)
			return nil, err
		}
		staged[name] = f.Name()
		_, err = f.Write(b)
		if err == nil {
			err = f.Chmod(0o755)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(f.Name(), 0o755)
		}
		if err != nil {
			removeStaged(staged)
			return nil, err
		}
	}
	if err := verifyStaged(ctx, staged, version); err != nil {
		return nil, err
	}
	return staged, nil
}

func verifyStaged(ctx context.Context, staged map[string]string, version string) error {
	for _, base := range []string{"masamong", "masamongd"} {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		args := []string{"version", "--output", "json"}
		if base == "masamongd" {
			args = []string{"--version"}
		}
		raw, err := exec.CommandContext(cctx, staged[exeName(base)], args...).Output()
		cancel()
		var v struct {
			Output struct {
				Version string `json:"version"`
			} `json:"output"`
		}
		if err != nil || json.Unmarshal(lastLine(raw), &v) != nil || strings.TrimPrefix(v.Output.Version, "v") != strings.TrimPrefix(version, "v") {
			removeStaged(staged)
			return fmt.Errorf("the new %s does not run or reports an unexpected version (expected %s): %v", base, version, err)
		}
	}
	return nil
}

func lastLine(b []byte) []byte {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	return lines[len(lines)-1]
}

func removeStaged(staged map[string]string) {
	for _, p := range staged {
		_ = os.Remove(p)
	}
}

// swapBinaries uses the same durable file-pair mechanism as restore. Recovery names
// are unique for each operation, so a mapped old Windows image cannot block a later swap.
func swapBinaries(dir string, staged map[string]string) error {
	lock, err := launcher.AcquireNamedLock(dir, ".masamong-update.lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	return swapBinariesLocked(dir, staged)
}

func swapBinariesLocked(dir string, staged map[string]string) error {
	journalPath := filepath.Join(dir, updatePending)
	if _, err := os.Lstat(journalPath); !os.IsNotExist(err) {
		return fmt.Errorf("a program update needs recovery before another update")
	}
	j, err := newMaintenance(dir, "update")
	if err != nil {
		return err
	}
	for _, name := range []string{exeName("masamong"), exeName("masamongd")} {
		target := filepath.Join(j.Root, name)
		stage := recoveryName(target, "new", j.ID)
		if _, err := os.Lstat(stage); !os.IsNotExist(err) {
			removePairStages(&j)
			return fmt.Errorf("staging path is already reserved: %s", stage)
		}
		if err := fsutil.RenameDurable(staged[name], stage); err != nil {
			removePairStages(&j)
			return err
		}
		f, err := planFile(target, stage, j.ID)
		if err != nil {
			os.Remove(stage)
			removePairStages(&j)
			return err
		}
		if f.OldHash == "" {
			os.Remove(stage)
			removePairStages(&j)
			return fmt.Errorf("installed program missing: %s", target)
		}
		j.Entries = append(j.Entries, f)
	}
	if err := saveMaintenance(journalPath, &j, "prepared"); err != nil {
		return err
	}
	err = installPair(journalPath, &j, func() error { return nil })
	if err != nil && !errors.Is(err, errMaintenanceInterrupted) && j.Phase != "committed" && j.Phase != "rolled_back" {
		if rb := rollbackPair(journalPath, &j); rb != nil {
			return errors.Join(err, fmt.Errorf("program rollback failed: %w", rb))
		}
	}
	return err
}

func removePairStages(j *maintenanceJournal) {
	for _, f := range j.Entries {
		if f.Stage != "" {
			_ = os.Remove(f.Stage)
		}
	}
}

// RecoverUpdateAt runs before a supervisor launches its next child, as well as before
// daemon startup. It never deletes old copies while a replacement is incomplete.
func RecoverUpdateAt(dir string) error {
	lock, err := launcher.AcquireNamedLock(dir, ".masamong-update.lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	return recoverUpdateLocked(dir)
}

// RollbackPendingUpdate is an offline operator recovery choice. Explicit rollback
// also permits a verified pair whose cleanup was interrupted and later lost a file.
func RollbackPendingUpdate() (bool, error) {
	dir, err := InstallDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, updatePending)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false, nil
	}
	lock, err := launcher.AcquireNamedLock(dir, ".masamong-update.lock")
	if err != nil {
		return true, err
	}
	defer lock.Release()
	j, err := readMaintenance(path, dir)
	if err != nil {
		return true, err
	}
	if j.Kind != "update" {
		return true, fmt.Errorf("invalid program maintenance journal")
	}
	if classifyUpdate(j) == updateStuck {
		return true, errUpdateStuck
	}
	return true, rollbackPair(path, j)
}

func pendingProgramMaintenance() (string, *maintenanceJournal, error) {
	dir, err := InstallDir()
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, updatePending)
	j, err := readMaintenance(path, dir)
	return path, j, err
}

func recoverUpdateLocked(dir string) error {
	p := filepath.Join(dir, updatePending)
	j, err := readMaintenance(p, dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Kind != "update" {
		return fmt.Errorf("invalid program maintenance journal")
	}
	err = installPair(p, j, func() error { return nil })
	if err != nil && !errors.Is(err, errMaintenanceInterrupted) && j.Phase != "committed" && j.Phase != "rolled_back" {
		switch state := classifyUpdate(j); state {
		case updateNewPair, updateOldPair:
			return settleUpdate(p, j, state) // the live files already are one complete pair
		case updateStuck:
			return fmt.Errorf("%w: %v", errUpdateStuck, err)
		}
		if rb := rollbackPair(p, j); rb != nil {
			return errors.Join(err, fmt.Errorf("program rollback failed: %w", rb))
		}
		return nil // successfully restored the complete old pair
	}
	return err
}

// CleanUpdateLeftovers first reconciles any unfinished pair, then retries unlocked
// recovery copies from completed Windows updates. A read-only installation needs no cleanup.
func CleanUpdateLeftovers() error {
	dir, err := InstallDir()
	if err != nil {
		return err
	}
	var leftovers []string
	for _, base := range []string{"masamong", "masamongd"} {
		for _, suffix := range []string{".old", ".old-*", ".new-*"} {
			files, _ := filepath.Glob(filepath.Join(dir, "."+exeName(base)+suffix))
			leftovers = append(leftovers, files...)
		}
	}
	if dl, _ := filepath.Glob(filepath.Join(dir, ".masamong-dl-*")); len(dl) > 0 {
		leftovers = append(leftovers, dl...)
	}
	_, pendingErr := os.Lstat(filepath.Join(dir, updatePending))
	if os.IsNotExist(pendingErr) && len(leftovers) == 0 {
		return nil
	}
	if abandoned, _ := filepath.Glob(filepath.Join(dir, ".masamong-update.abandoned-*.json")); len(abandoned) > 0 && os.IsNotExist(pendingErr) {
		return nil // the operator abandoned an update to keep every file: nothing is deleted until they remove that record
	}
	lock, err := launcher.AcquireNamedLock(dir, ".masamong-update.lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	if !os.IsNotExist(pendingErr) {
		if err := recoverUpdateLocked(dir); err != nil {
			return err
		}
	}
	for _, base := range []string{"masamong", "masamongd"} {
		target := filepath.Join(dir, exeName(base))
		if st, err := os.Stat(target); err != nil || !st.Mode().IsRegular() {
			return nil
		}
	}
	for _, base := range []string{"masamong", "masamongd"} {
		for _, suffix := range []string{".old", ".old-*", ".new-*"} {
			files, _ := filepath.Glob(filepath.Join(dir, "."+exeName(base)+suffix))
			for _, p := range files {
				_ = os.Remove(p)
			}
		}
	}
	if dl, _ := filepath.Glob(filepath.Join(dir, ".masamong-dl-*")); len(dl) > 0 {
		for _, p := range dl {
			_ = os.Remove(p)
		}
	}
	return nil
}
