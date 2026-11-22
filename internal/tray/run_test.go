//go:build (darwin && cgo) || windows || linux

package tray

import (
	"context"
	"testing"
)

// The icon's "Open console" builds a sign-in link only through launcher.Open, which proves the recorded
// address with the token: the options carry both, or Open refuses to build a link.
func TestOpenOptionsCarryTheTokenThatProvesTheConsole(t *testing.T) {
	app := &trayApp{ctx: context.Background(), o: Options{DataRoot: t.TempDir(),
		Token:   func() (string, error) { return "synthetic-token", nil },
		OnceURL: func(string) (string, error) { return "http://x/login?once=1", nil }}}
	for _, browser := range []bool{true, false} {
		o := app.openOptions(browser)
		if o.OnceURL == nil || o.Token == nil || o.NoBrowser == browser || o.DataRoot != app.o.DataRoot {
			t.Fatalf("browser=%v: %+v", browser, o)
		}
		if tok, err := o.Token(); err != nil || tok != "synthetic-token" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
}
