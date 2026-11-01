// Package launcher is what the Masamong icon runs: find the instance, reuse a running daemon or start
// one in the background, wait until the console answers, and open the default browser already signed
// in (one-time code, never the long-lived token in a URL). Closing the browser never stops the bot.
package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"masamong/internal/fsutil"
)

// DefaultDataRoot is where a clicked launcher keeps its instance when none is given.
func DefaultDataRoot() string {
	if v := os.Getenv("MASAMONG_DATA_ROOT"); v != "" {
		return v
	}
	return OSDataRoot()
}

// OSDataRoot is the platform's default data folder, ignoring MASAMONG_DATA_ROOT.
func OSDataRoot() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Masamong")
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "Masamong")
		}
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "masamong")
	}
	return filepath.Join(home, ".local", "share", "masamong")
}

// DaemonInfo is written by masamongd so launchers can find the running console.
type DaemonInfo struct {
	PID int `json:"pid"`
	// Supervisor is the masamongd --supervise process that restarts this one after a crash (0 = none).
	Supervisor int    `json:"supervisor,omitempty"`
	Bind       string `json:"bind"`
	Started    string `json:"started"`
	Version    string `json:"version"`
}

func infoPath(dataRoot string) string { return filepath.Join(dataRoot, "daemon.json") }

// WriteInfo records the running daemon (0600).
func WriteInfo(dataRoot string, info DaemonInfo) error {
	if err := fsutil.PrivateDir(dataRoot); err != nil {
		return err
	}
	raw, _ := json.Marshal(info)
	return fsutil.WriteAtomic(infoPath(dataRoot), raw, 0o600)
}

// RemoveInfo is called on clean shutdown.
func RemoveInfo(dataRoot string) { _ = os.Remove(infoPath(dataRoot)) }

// ReadInfo returns the recorded daemon, if any.
func ReadInfo(dataRoot string) (DaemonInfo, bool) {
	raw, err := os.ReadFile(infoPath(dataRoot))
	if err != nil {
		return DaemonInfo{}, false
	}
	var info DaemonInfo
	return info, json.Unmarshal(raw, &info) == nil && info.Bind != ""
}

// InMaintenance reports whether a Health answer is from a daemon stopped for maintenance: it answers
// (so it is not "not ready"), its console is a plain instructions page, and the bot and schedulers are off.
func InMaintenance(health map[string]any) bool { return health["maintenance"] == true }

// Health asks a console whether it is up and which instance it serves.
func Health(ctx context.Context, bind string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+hostPort(bind)+"/api/health", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out); err != nil {
		return nil, err
	}
	if out["ok"] != true {
		return nil, errors.New("console not ready")
	}
	return out, nil
}

// hostPort turns a bind address into something a client can dial.
func hostPort(bind string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		return bind
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// Options control Open.
type Options struct {
	DataRoot  string
	Daemon    string // masamongd path; empty = next to this executable, then $PATH
	NoBrowser bool
	Wait      time.Duration
	Log       io.Writer                         // progress lines for a terminal; nil = silent
	OnceURL   func(bind string) (string, error) // builds the one-time sign-in link
}

// Result says what Open did.
type Result struct {
	URL      string `json:"url"`
	Started  bool   `json:"started"` // false = an already running daemon was reused
	Bind     string `json:"bind"`
	LogFile  string `json:"log_file,omitempty"`
	Browser  bool   `json:"browser_opened"`
	DataRoot string `json:"data_root"`
	// Maintenance is true when the daemon is stopped for maintenance: URL is its instructions page (no
	// sign-in), not the console.
	Maintenance bool `json:"maintenance,omitempty"`
}

func (o Options) say(format string, args ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", args...)
	}
}

// Open implements the icon click.
func Open(ctx context.Context, o Options) (Result, error) {
	res := Result{DataRoot: o.DataRoot}
	if err := fsutil.PrivateDir(o.DataRoot); err != nil {
		return res, fmt.Errorf("data folder %s: %w", o.DataRoot, err)
	}
	if o.Wait == 0 {
		o.Wait = 25 * time.Second
	}
	bind := ""
	if info, ok := ReadInfo(o.DataRoot); ok {
		if h, err := Health(ctx, info.Bind); err == nil {
			bind = info.Bind
			res.Maintenance = InMaintenance(h)
			o.say("masamong is already running (%s)", hostPort(bind))
		}
	}
	if bind == "" {
		daemon, err := findDaemon(o.Daemon)
		if err != nil {
			return res, err
		}
		logDir := filepath.Join(o.DataRoot, "logs")
		if err := fsutil.PrivateDir(logDir); err != nil {
			return res, fmt.Errorf("log folder %s: %w", logDir, err)
		}
		// masamongd writes its own rotating logs/daemon.log; its stdout/stderr (a crash trace, if any)
		// go to daemon.out, started afresh once it passes 5 MiB so it stays bounded too
		res.LogFile = filepath.Join(logDir, "daemon.log")
		outPath := filepath.Join(logDir, "daemon.out")
		flags := os.O_CREATE | os.O_APPEND | os.O_WRONLY
		if st, err := os.Stat(outPath); err == nil && st.Size() > 5<<20 {
			flags = os.O_CREATE | os.O_TRUNC | os.O_WRONLY
		}
		lf, err := os.OpenFile(outPath, flags, 0o600)
		if err != nil {
			return res, err
		}
		if err := fsutil.PrivateFile(outPath); err != nil {
			_ = lf.Close()
			return res, fmt.Errorf("protect daemon output log %s: %w", outPath, err)
		}
		// --supervise: a crash is answered by starting the daemon again (see cmd/masamongd/supervise.go)
		cmd := exec.Command(daemon, "--supervise", "--data-root", o.DataRoot, "--web")
		cmd.Stdout, cmd.Stderr = lf, lf
		detach(cmd)
		o.say("starting masamong in the background…")
		if err := cmd.Start(); err != nil {
			_ = lf.Close()
			return res, fmt.Errorf("start %s: %w", daemon, err)
		}
		_ = lf.Close()
		_ = cmd.Process.Release()
		res.Started = true
		deadline := time.Now().Add(o.Wait)
		for time.Now().Before(deadline) {
			if info, ok := ReadInfo(o.DataRoot); ok {
				if h, err := Health(ctx, info.Bind); err == nil {
					bind = info.Bind
					res.Maintenance = InMaintenance(h)
					break
				}
			}
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		if bind == "" {
			return res, fmt.Errorf("masamong did not become ready in %s; see %s", o.Wait, res.LogFile)
		}
	}
	res.Bind = hostPort(bind)
	url := "http://" + res.Bind + "/"
	if res.Maintenance {
		o.say("masamong is stopped for maintenance: opening the page that says what to do")
	} else if o.OnceURL != nil {
		if u, err := o.OnceURL(bind); err == nil {
			url = u
		}
	}
	res.URL = url
	if !o.NoBrowser {
		if err := OpenBrowser(url); err == nil {
			res.Browser = true
		} else {
			o.say("could not open a browser: %v", err)
		}
	}
	return res, nil
}

func findDaemon(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	name := "masamongd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", errors.New("masamongd not found next to this program or on PATH")
}

// OpenBrowser opens url in the user's default browser.
func OpenBrowser(url string) error {
	if os.Getenv("MASAMONG_NO_BROWSER") != "" {
		return errors.New("MASAMONG_NO_BROWSER is set")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// ConnectOptions control Connect.
type ConnectOptions struct {
	Host           string // user@host, as ssh understands it
	RemoteDataRoot string // empty = the remote default
	RemoteCommand  string // masamong on the remote side (default "masamong")
	LocalPort      int    // 0 = pick a free one
	Log            io.Writer
}

// Connect manages a remote instance from this computer's browser: it asks the remote launcher to
// start (or reuse) the daemon and mint a one-time link, forwards a local port to the remote console
// over SSH, opens the local browser, and keeps the tunnel until ctx ends. Nothing listens publicly.
func Connect(ctx context.Context, o ConnectOptions) error {
	if o.Host == "" {
		return errors.New("host required (user@host)")
	}
	if strings.HasPrefix(o.Host, "-") || strings.ContainsAny(o.Host, " \t\n") {
		// ssh would read "-oProxyCommand=…" as an option and run it
		return fmt.Errorf("%q is not a host (user@host)", o.Host)
	}
	if o.RemoteCommand == "" {
		// `ssh host cmd` runs a non-login shell, which usually lacks the per-user install folder
		o.RemoteCommand = `PATH="$HOME/.local/bin:/usr/local/bin:$PATH" masamong`
	}
	say := func(f string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, f+"\n", a...)
		}
	}
	remote := []string{o.RemoteCommand}
	if o.RemoteDataRoot != "" {
		remote = append(remote, "--data-root", shellQuote(o.RemoteDataRoot))
	}
	remote = append(remote, "open", "--no-browser", "--output", "json")
	say("asking %s to start masamong…", o.Host)
	var errOut strings.Builder
	cmd := exec.CommandContext(ctx, "ssh", o.Host, strings.Join(remote, " "))
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return fmt.Errorf("remote open failed: %w: %s", err, msg)
		}
		return fmt.Errorf("remote open failed: %w", err)
	}
	var r struct {
		OK     bool `json:"ok"`
		Output struct {
			URL  string `json:"url"`
			Bind string `json:"bind"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Output.URL == "" {
		return fmt.Errorf("unexpected reply from remote masamong: %s", strings.TrimSpace(string(out)))
	}
	_, rport, err := net.SplitHostPort(r.Output.Bind)
	if err != nil {
		return err
	}
	lport := o.LocalPort
	if lport == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		lport = l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
	}
	tunnel := exec.CommandContext(ctx, "ssh", "-N", "-o", "ExitOnForwardFailure=yes",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%s", lport, rport), o.Host)
	if err := tunnel.Start(); err != nil {
		return err
	}
	local := fmt.Sprintf("127.0.0.1:%d", lport)
	healthy := false
	for i := 0; i < 40; i++ {
		if _, err := Health(ctx, local); err == nil {
			healthy = true
			break
		}
		select {
		case <-ctx.Done():
			_ = tunnel.Process.Kill()
			_ = tunnel.Wait()
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if !healthy {
		_ = tunnel.Process.Kill()
		_ = tunnel.Wait()
		return fmt.Errorf("SSH tunnel opened but the remote console did not become healthy at %s", local)
	}
	url := strings.Replace(r.Output.URL, hostPort(r.Output.Bind), local, 1)
	if err := OpenBrowser(url); err != nil {
		say("open this address in your browser: %s", url)
	} else {
		say("opened the console at http://%s — press Ctrl+C to close the tunnel (the bot keeps running)", local)
	}
	return tunnel.Wait()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Running reports whether a named lock (tray.lock) is held by another process.
func Running(dataRoot, lockName string) bool { return lockHeld(dataRoot, lockName) }

// errLockHeld is a single-owner lock that another process holds.
var errLockHeld = errors.New("another masamong is already running")

// acquireRetrying retries a held lock for a moment: a Running check can hold it for an instant, and
// an owner starting just then must not exit thinking another copy runs.
func acquireRetrying(try func() (*Lock, error)) (*Lock, error) {
	var err error
	for i := 0; i < 10; i++ {
		var l *Lock
		if l, err = try(); err == nil || !errors.Is(err, errLockHeld) {
			return l, err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, err
}

// Spawn starts a detached copy of this program with args (the tray from `open`).
func Spawn(args ...string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, args...)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// DesktopSession reports whether a graphical session is available for icons and browsers.
func DesktopSession() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return os.Getenv("SSH_CONNECTION") == "" || os.Getenv("DISPLAY") != ""
	}
	return desktopSession()
}
