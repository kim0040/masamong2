package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"

	"masamong/internal/action"
	"masamong/internal/launcher"
	"masamong/internal/transport/web"
	"masamong/internal/tray"
)

// stopTray is replaced in tests.
var stopTray = launcher.StopTray

// launchCommand runs the commands that manage the service process rather than an instance Action:
//
//	open      start (or reuse) the service and open the console, signed in; shows the menu bar icon
//	start     start (or reuse) the service without a browser
//	stop      stop the service cleanly
//	restart   restart the service
//	status    is it running, where, paused?
//	tray      the menu bar / notification area icon (starts the service)
//	connect   manage a remote computer over SSH from this browser
func launchCommand(ctx context.Context, act string, opt Options, input json.RawMessage, stdout, stderr io.Writer, stdoutTTY bool) int {
	in := map[string]any{}
	_ = json.Unmarshal(input, &in)
	flag := func(k string) bool { v, _ := in[k].(bool); return v }
	str := func(k string) string { v, _ := in[k].(string); return v }
	var progress io.Writer
	if stdoutTTY && opt.Output != "json" {
		progress = stderr
	}
	if act == "launcher.connect" {
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		port := 0
		if v, ok := in["port"].(float64); ok {
			port = int(v)
		}
		if err := launcher.Connect(ctx, launcher.ConnectOptions{Host: str("host"), RemoteDataRoot: str("remote_data_root"),
			RemoteCommand: str("remote_command"), LocalPort: port, Log: stderr}); err != nil && ctx.Err() == nil {
			fmt.Fprintln(stderr, "masamong connect:", err)
			return 1
		}
		return 0
	}
	root := opt.DataRoot
	if root == "" {
		root = launcher.DefaultDataRoot()
	}
	onceURL := func(bind string) (string, error) {
		code, err := web.CreateOnce(root)
		if err != nil {
			return "", err
		}
		return web.OnceURL(bind, code), nil
	}
	token := func() (string, error) { return web.EnsureToken(root) }
	emit := func(res action.Result, human string) int {
		if opt.Output == "json" || !stdoutTTY {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(res)
		} else if res.OK && human != "" {
			fmt.Fprintln(stdout, human)
		}
		if !res.OK {
			if res.Error != nil {
				fmt.Fprintf(stderr, "masamong %s: %s\n", act[len("launcher."):], res.Error.Message)
			}
			return 1
		}
		return 0
	}
	fail := func(err error) action.Result {
		return action.Result{OK: false, Action: act, Error: &action.Error{Code: "IO", Message: err.Error()}}
	}
	withTray := func() {
		// the icon comes with the console on a desktop; `--no-tray` skips it
		if !flag("no_tray") && tray.Supported() && !launcher.Running(root, "tray.lock") {
			_ = launcher.Spawn("--data-root", root, "tray")
		}
	}

	switch act {
	case "launcher.stop":
		// the icon is a courtesy: the service is stopped even when the icon cannot be closed
		if err := stopTray(root); err != nil {
			fmt.Fprintf(stderr, "masamong stop: the notification-area icon could not be closed (%v); stopping the service anyway\n", err)
		}
		tok, err := token()
		if err != nil {
			return emit(fail(err), "")
		}
		stopped, err := launcher.Stop(ctx, root, tok, "cli")
		if err != nil {
			return emit(fail(err), "")
		}
		msg := "masamong was not running"
		if stopped {
			msg = "masamong stopped"
		}
		return emit(action.Result{OK: true, Action: act, Output: map[string]any{"stopped": stopped, "data_root": root}}, msg)
	case "launcher.restart":
		if info, ok := launcher.ReadInfo(root); ok {
			if _, err := launcher.Health(ctx, info.Bind); err == nil {
				tok, err := token()
				if err != nil {
					return emit(fail(err), "")
				}
				c := launcher.NewClient(info.Bind, tok)
				_, err = c.Action(ctx, "runtime.restart", nil)
				c.Close(ctx) // kept across the restart otherwise; a no-op once the listener is gone
				if err != nil {
					return emit(fail(err), "")
				}
				return emit(action.Result{OK: true, Action: act, Output: map[string]any{"restarting": true, "bind": info.Bind}}, "masamong is restarting")
			}
		}
		act = "launcher.start" // not running: start it
		fallthrough
	case "launcher.start", "launcher.open":
		res, err := launcher.Open(ctx, launcher.Options{DataRoot: root, NoBrowser: act == "launcher.start" || flag("no_browser"), Log: progress, OnceURL: onceURL})
		out := action.Result{OK: err == nil, Action: act, Output: map[string]any{"url": res.URL, "bind": res.Bind, "started": res.Started,
			"browser_opened": res.Browser, "data_root": res.DataRoot, "log_file": res.LogFile, "maintenance": res.Maintenance}}
		if res.Maintenance {
			out.Output["message"] = "masamong is stopped for maintenance"
		}
		if err != nil {
			out.Error = &action.Error{Code: "IO", Message: err.Error()}
			return emit(out, "")
		}
		withTray()
		human := fmt.Sprintf("masamong is running at http://%s", res.Bind)
		if res.Maintenance {
			human = "masamong is stopped for maintenance"
		}
		if act == "launcher.open" {
			if !res.Maintenance {
				human = fmt.Sprintf("opened the console at http://%s", res.Bind)
				if !res.Browser {
					human = fmt.Sprintf("open this address (valid for 2 minutes): %s", res.URL)
				}
			}
		}
		return emit(out, human)
	case "launcher.status":
		info, ok := launcher.ReadInfo(root)
		out := map[string]any{"running": false, "data_root": root, "autostart": launcher.AutostartStatus(root).Enabled, "maintenance": false}
		if ok {
			if h, err := launcher.Health(ctx, info.Bind); err == nil {
				out["running"], out["bind"], out["pid"], out["version"], out["started"], out["ready"] = true, info.Bind, info.PID, info.Version, info.Started, h["ready"]
				out["maintenance"] = launcher.InMaintenance(h)
				if info.Supervisor > 0 {
					out["supervisor_pid"] = info.Supervisor
				}
				if tok, err := token(); err == nil && out["maintenance"] != true {
					c := launcher.NewClient(info.Bind, tok)
					st, err := c.Action(ctx, "runtime.status", nil)
					c.Close(ctx)
					if err == nil {
						if st["maintenance"] == true {
							out["maintenance"] = true
						}
						if p, ok := st["pause"].(map[string]any); ok {
							out["paused"] = p["all"]
						}
					}
				}
			}
		}
		human := "masamong is not running (start it with: masamong start)"
		if out["running"] == true {
			if out["maintenance"] == true {
				human = "masamong is stopped for maintenance"
				out["message"] = human
			} else {
				human = fmt.Sprintf("masamong is running at http://%v (pid %v, version %v)", out["bind"], out["pid"], out["version"])
				if out["paused"] == true {
					human += " — replies are paused"
				}
			}
		}
		return emit(action.Result{OK: true, Action: act, Output: out}, human)
	case "launcher.tray":
		if !tray.Supported() {
			// no icon in this build or session: behave like start
			res, err := launcher.Open(ctx, launcher.Options{DataRoot: root, NoBrowser: true, Log: progress, OnceURL: onceURL})
			if err != nil {
				return emit(fail(err), "")
			}
			return emit(action.Result{OK: true, Action: act, Output: map[string]any{"bind": res.Bind, "icon": false}}, "no menu bar icon here; masamong is running at http://"+res.Bind)
		}
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		if err := tray.Run(ctx, tray.Options{DataRoot: root, Lang: opt.Locale, Token: token, OnceURL: onceURL}); err != nil {
			if err == tray.ErrRunning {
				return 0
			}
			return emit(fail(err), "")
		}
		return 0
	}
	return emit(fail(fmt.Errorf("unknown command")), "")
}
