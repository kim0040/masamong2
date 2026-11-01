//go:build !noweb

package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// tailnetName is this computer's Tailscale name (MagicDNS), read from `tailscale status --json`, so the
// console can offer to answer to it (admin.web.hosts) for tailscale serve. The look-up runs in the
// background at most every few minutes; until it finishes, or without Tailscale, the name is "".
type tailnetName struct {
	mu      sync.Mutex
	name    string
	checked time.Time
	running bool
	look    func(context.Context) string // tests replace it
}

const tailnetRecheck = 5 * time.Minute

func (t *tailnetName) get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running && time.Since(t.checked) >= tailnetRecheck {
		t.running = true
		look := t.look
		if look == nil {
			look = lookTailnet
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			n := look(ctx)
			t.mu.Lock()
			t.name, t.checked, t.running = n, time.Now(), false
			t.mu.Unlock()
		}()
	}
	return t.name
}

func lookTailnet(ctx context.Context) string {
	exe, err := exec.LookPath("tailscale")
	if err != nil {
		for _, p := range tailscalePaths() {
			if st, e := os.Stat(p); e == nil && !st.IsDir() {
				exe, err = p, nil
				break
			}
		}
	}
	if err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, exe, "status", "--json")
	noWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var st struct {
		BackendState string
		Self         struct{ DNSName string }
	}
	if json.Unmarshal(out, &st) != nil || st.BackendState != "Running" {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(st.Self.DNSName), ".")
}

func tailscalePaths() []string {
	switch runtime.GOOS {
	case "windows":
		return []string{os.Getenv("ProgramFiles") + `\Tailscale\tailscale.exe`}
	case "darwin":
		return []string{"/Applications/Tailscale.app/Contents/MacOS/Tailscale"}
	}
	return nil
}
