//go:build (darwin && cgo) || windows || linux

package tray

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"

	"masamong/internal/launcher"
)

//go:embed icons/normal.png
var iconNormal []byte

//go:embed icons/warn.png
var iconWarn []byte

//go:embed icons/paused.png
var iconPaused []byte

//go:embed icons/error.png
var iconError []byte

//go:embed icons/app-32.png
var iconApp []byte

// Supported reports whether this build can show an icon in this session.
func Supported() bool { return launcher.DesktopSession() }

// ErrRunning means another tray already serves this data folder.
var ErrRunning = errors.New("the Masamong icon is already running")

// Run shows the icon until the operator quits it. It starts the service when it is not running.
func Run(ctx context.Context, o Options) error {
	lock, err := launcher.AcquireNamedLock(o.DataRoot, "tray.lock")
	if err != nil {
		return ErrRunning
	}
	defer lock.Release()
	if o.Lang == "" {
		o.Lang = SystemLang()
	}
	t := &trayApp{o: o, tx: texts(o.Lang), ctx: ctx}
	systray.Run(t.ready, func() {})
	return nil
}

// state is what the icon shows.
type state int

const (
	stateDown   state = iota
	stateOK           // running and answering
	stateSetup        // running, but the first-run setup is not finished
	statePaused       // running with every reply paused
)

type trayApp struct {
	o   Options
	tx  map[string]string
	ctx context.Context

	mu     sync.Mutex // ensure/refresh run on their own goroutines
	client *launcher.Client
	bind   string
	paused bool

	status, open, pause, restart, autostart, stop, quit *systray.MenuItem
}

func (t *trayApp) s(k string) string {
	if v := t.tx["tray."+k]; v != "" {
		return v
	}
	return k
}

func icoFromPNG(png []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, []uint16{0, 1, 1})
	b.Write([]byte{32, 32, 0, 0})
	_ = binary.Write(&b, binary.LittleEndian, []uint16{1, 32})
	_ = binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(png)), 22})
	b.Write(png)
	return b.Bytes()
}

// setIcon picks the state icon. macOS draws the monochrome template set; Windows and Linux trays show
// the app icon and carry the state in the status line and tooltip.
func (t *trayApp) setIcon(st state) {
	switch runtime.GOOS {
	case "darwin":
		icon := map[state][]byte{stateOK: iconNormal, statePaused: iconPaused, stateDown: iconError, stateSetup: iconWarn}[st]
		systray.SetTemplateIcon(icon, icon)
	case "windows":
		systray.SetIcon(icoFromPNG(iconApp))
	default:
		systray.SetIcon(iconApp)
	}
}

func (t *trayApp) ready() {
	t.setIcon(stateOK)
	systray.SetTooltip(t.s("tooltip"))
	t.status = systray.AddMenuItem(t.s("starting"), "")
	t.status.Disable()
	systray.AddSeparator()
	t.open = systray.AddMenuItem(t.s("open"), "")
	t.pause = systray.AddMenuItem(t.s("pause"), "")
	t.restart = systray.AddMenuItem(t.s("restart"), "")
	systray.AddSeparator()
	t.autostart = systray.AddMenuItemCheckbox(t.s("autostart"), "", launcher.AutostartStatus(t.o.DataRoot).Enabled)
	systray.AddSeparator()
	t.stop = systray.AddMenuItem(t.s("stop"), "")
	t.quit = systray.AddMenuItem(t.s("quit"), "")
	go t.ensure(false)
	go t.loop()
}

// ensure starts (or finds) the service; browser=true also opens the console.
func (t *trayApp) ensure(browser bool) {
	res, err := launcher.Open(t.ctx, launcher.Options{DataRoot: t.o.DataRoot, NoBrowser: !browser, OnceURL: t.o.OnceURL})
	if err != nil {
		t.show(stateDown)
		return
	}
	t.mu.Lock()
	t.bind = res.Bind
	if tok, err := t.o.Token(); err == nil {
		t.client = launcher.NewClient(res.Bind, tok)
		t.client.Via = "tray"
	}
	t.mu.Unlock()
	t.refresh()
}

// conn returns the current console connection, finding a running service when there is none yet.
func (t *trayApp) conn() (string, *launcher.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.bind == "" {
		if info, ok := launcher.ReadInfo(t.o.DataRoot); ok {
			t.bind = info.Bind
			if tok, err := t.o.Token(); err == nil {
				t.client = launcher.NewClient(info.Bind, tok)
				t.client.Via = "tray"
			}
		}
	}
	return t.bind, t.client
}

func (t *trayApp) refresh() {
	bind, client := t.conn()
	if bind == "" || client == nil {
		t.show(stateDown)
		return
	}
	h, err := launcher.Health(t.ctx, bind)
	if err != nil {
		t.show(stateDown)
		return
	}
	if h["ready"] == false {
		t.show(stateSetup)
		return
	}
	out, err := client.Action(t.ctx, "runtime.status", nil)
	if err == nil {
		if p, ok := out["pause"].(map[string]any); ok && p["all"] == true {
			t.show(statePaused)
			return
		}
	}
	t.show(stateOK)
}

func (t *trayApp) show(st state) {
	t.mu.Lock()
	t.paused = st == statePaused
	if st == stateDown {
		t.bind, t.client = "", nil
	}
	t.mu.Unlock()
	t.setIcon(st)
	switch st {
	case stateDown:
		t.status.SetTitle(t.s("down"))
		t.open.SetTitle(t.s("start"))
		t.pause.Disable()
		t.restart.Disable()
		t.stop.Disable()
		return
	case stateSetup:
		t.status.SetTitle(t.s("setup"))
		t.pause.Disable()
	case statePaused:
		t.status.SetTitle(t.s("paused"))
		t.pause.SetTitle(t.s("resume"))
		t.pause.Enable()
	default:
		t.status.SetTitle(t.s("running"))
		t.pause.SetTitle(t.s("pause"))
		t.pause.Enable()
	}
	t.open.SetTitle(t.s("open"))
	t.restart.Enable()
	t.stop.Enable()
}

func (t *trayApp) loop() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.ctx.Done():
			systray.Quit()
			return
		case <-tick.C:
			go t.refresh()
		case <-t.open.ClickedCh:
			go t.ensure(true)
		case <-t.pause.ClickedCh:
			_, client := t.conn()
			t.mu.Lock()
			paused := t.paused
			t.mu.Unlock()
			if client != nil {
				go func() {
					_, _ = client.Action(t.ctx, "runtime.pause", map[string]any{"scope": "all", "paused": !paused})
					t.refresh()
				}()
			}
		case <-t.restart.ClickedCh:
			if _, client := t.conn(); client != nil {
				t.status.SetTitle(t.s("restarting"))
				go func() {
					_, _ = client.Action(t.ctx, "runtime.restart", nil)
					time.Sleep(3 * time.Second)
					t.refresh()
				}()
			}
		case <-t.autostart.ClickedCh:
			st, err := launcher.SetAutostart(t.o.DataRoot, !t.autostart.Checked())
			if err == nil && st.Enabled {
				t.autostart.Check()
			} else {
				t.autostart.Uncheck()
			}
		case <-t.stop.ClickedCh:
			go func() {
				if tok, err := t.o.Token(); err == nil {
					_, _ = launcher.Stop(t.ctx, t.o.DataRoot, tok, "tray")
				}
				t.show(stateDown)
			}()
		case <-t.quit.ClickedCh:
			// closes only the icon: the service keeps running (Stop Masamong ends it)
			systray.Quit()
			return
		}
	}
}
