package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"masamong/internal/action"
)

// Client talks to a running console on this computer with the bootstrap token (the tray, `masamong
// stop`). It signs in like a browser and sends the CSRF header on every Action.
type Client struct {
	Base  string
	Via   string // "cli" (default) or "tray"
	token string
	http  *http.Client
	csrf  string
	// recheck runs after a rejected sign-in, before that action is sent again.
	recheck func(context.Context) error
}

// NewClient prepares a client for the console at bind.
func NewClient(bind, token string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{Base: "http://" + hostPort(bind), token: token, Via: "cli", http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

// ViaHeader names the program behind a launcher request ("cli" or "tray") so the change log does not
// record a `masamong stop` or the icon's menu as the console.
const ViaHeader = "X-Masamong-Via"

func (c *Client) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"token": c.token})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var out struct {
		OK   bool   `json:"ok"`
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&out); err != nil || !out.OK {
		return fmt.Errorf("sign-in refused (%d)", res.StatusCode)
	}
	c.csrf = out.CSRF
	return nil
}

// Close signs this client out, so a one-shot command (status, restart) leaves no session behind.
func (c *Client) Close(ctx context.Context) {
	if c.csrf == "" {
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/logout", nil)
	req.Header.Set("X-CSRF-Token", c.csrf)
	if res, err := c.http.Do(req); err == nil {
		_ = res.Body.Close()
	}
	c.csrf = ""
}

// ErrActionFailed carries the console's error message.
var ErrActionFailed = errors.New("action failed")

// Action runs one Application Action and returns its output.
func (c *Client) Action(ctx context.Context, id string, input map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	out, err := c.Execute(ctx, action.Request{Action: id, Input: raw})
	if err != nil {
		return nil, err
	}
	if !out.OK {
		msg := ""
		if out.Error != nil {
			msg = out.Error.Message
		}
		return out.Output, fmt.Errorf("%w: %s", ErrActionFailed, msg)
	}
	return out.Output, nil
}

// Execute preserves the Action envelope, including revision, job and structured errors, for CLI parity.
// Only authentication failures are retried: an interrupted mutation must never be sent twice.
func (c *Client) Execute(ctx context.Context, request action.Request) (action.Result, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if c.csrf == "" {
			if err := c.login(ctx); err != nil {
				return action.Result{}, err
			}
		}
		body, err := json.Marshal(request)
		if err != nil {
			return action.Result{}, err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/action", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", c.csrf)
		req.Header.Set(ViaHeader, c.Via)
		req.Header.Set("X-Lang", request.Locale)
		// Imports, backups and chat can legitimately take longer than the launcher's short timeout.
		client := *c.http
		client.Timeout = 0
		res, err := client.Do(req)
		if err != nil {
			return action.Result{}, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, 64<<20))
		res.Body.Close()
		if readErr != nil {
			return action.Result{}, readErr
		}
		if res.StatusCode == http.StatusUnauthorized || (res.StatusCode == http.StatusForbidden && bytes.Contains(raw, []byte("csrf"))) {
			c.csrf = "" // the session was revoked, expired, or lost in an unclean daemon restart
			if c.recheck != nil {
				if err := c.recheck(ctx); err != nil {
					return action.Result{}, err
				}
			}
			continue
		}
		var out action.Result
		if err := json.Unmarshal(raw, &out); err != nil {
			return action.Result{}, fmt.Errorf("unexpected reply (%d)", res.StatusCode)
		}
		out.RequestID = request.RequestID
		if out.Error != nil {
			out.Error.RequestID = request.RequestID
		}
		return out, nil
	}
	return action.Result{}, errors.New("sign-in expired")
}

// Stop asks the daemon for dataRoot to shut down and waits until it is gone.
func Stop(ctx context.Context, dataRoot, token, via string) (bool, error) {
	info, ok := ReadInfo(dataRoot)
	if !ok {
		return false, nil
	}
	health, err := Health(ctx, info.Bind)
	if err != nil {
		if Running(dataRoot, "daemon.lock") {
			// busy or starting up, not gone: its record is not stale, and it must not be deleted
			return false, errors.New("the service is running but did not answer; wait a moment and try again")
		}
		RemoveInfo(dataRoot) // stale record
		return false, nil
	}
	if err := requestStop(ctx, info, health, token, via, ""); err != nil {
		return false, err
	}
	// The console goes away first; wait until the data folder is released as well.
	deadline := time.Now().Add(StopGrace)
	for time.Now().Before(deadline) {
		if _, err := Health(ctx, info.Bind); err != nil && !Running(dataRoot, "daemon.lock") {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return true, errors.New("the service did not stop within 40 seconds")
}

// StopGrace covers the daemon's reply and job draining on every platform.
const StopGrace = 40 * time.Second

// RequestStopChild uses the same console path as Stop, but never removes stale records or
// follows another PID. The supervisor waits on its actual child, not a port or lock.
func RequestStopChild(ctx context.Context, dataRoot string, pid int, token string) error {
	info, ok := ReadInfo(dataRoot)
	if !ok {
		return errors.New("child console record unavailable")
	}
	if pid <= 0 || info.PID != pid {
		return errors.New("console record belongs to another process")
	}
	host, _, err := net.SplitHostPort(hostPort(info.Bind))
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("child console is not on loopback")
	}
	health, err := Health(ctx, info.Bind)
	if err != nil {
		return errors.New("child console did not answer")
	}
	return requestStop(ctx, info, health, token, "cli", dataRoot)
}

func requestStop(ctx context.Context, info DaemonInfo, health map[string]any, token, via, expectedRoot string) error {
	if InMaintenance(health) {
		// the maintenance daemon has no sign-in and no Actions: it stops through its own endpoint
		if err := stopMaintenance(ctx, info.Bind, token, expectedRoot, info.PID); err != nil {
			return err
		}
	} else {
		c := NewClient(info.Bind, token)
		c.Via = via
		if expectedRoot != "" {
			root := expectedRoot
			c.recheck = func(ctx context.Context) error { return c.checkDataRoot(ctx, root) }
			if err := c.checkDataRoot(ctx, expectedRoot); err != nil {
				c.Close(ctx)
				return err
			}
		}
		if _, err := c.Action(ctx, "runtime.stop", nil); err != nil {
			c.Close(ctx)
			return err
		}
	}
	return nil
}

// checkDataRoot guards against a stale port now occupied by another instance, even if its
// authentication file was copied. Do not log either the token or the response body.
func (c *Client) checkDataRoot(ctx context.Context, expected string) error {
	if err := c.login(ctx); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/session", nil)
	res, err := c.http.Do(req)
	if err != nil {
		return errors.New("child console identity unavailable")
	}
	defer res.Body.Close()
	var out struct {
		Authenticated bool   `json:"authenticated"`
		DataRoot      string `json:"data_root"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&out); err != nil || !out.Authenticated || out.DataRoot == "" {
		return errors.New("child console identity unavailable")
	}
	want, err := filepath.Abs(expected)
	if err != nil {
		return errors.New("child data folder invalid")
	}
	got, err := filepath.Abs(out.DataRoot)
	if err != nil {
		return errors.New("console data folder invalid")
	}
	if got != want && !(runtime.GOOS == "windows" && strings.EqualFold(got, want)) {
		return errors.New("console belongs to another data folder")
	}
	return nil
}

// stopMaintenance asks a daemon stopped for maintenance to end (POST /api/maintenance/stop, which takes
// the admin token like a sign-in).
func stopMaintenance(ctx context.Context, bind, token, expectedRoot string, pid int) error {
	in := map[string]any{"token": token}
	if expectedRoot != "" {
		in["data_root"], in["pid"] = expectedRoot, pid
	}
	body, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+hostPort(bind)+"/api/maintenance/stop", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the maintenance console refused to stop (%d)", res.StatusCode)
	}
	return nil
}
