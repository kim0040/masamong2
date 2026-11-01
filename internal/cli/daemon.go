package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/errcode"
	"masamong/internal/i18n"
	"masamong/internal/launcher"
	"masamong/internal/transport/web"
	"masamong/internal/tui"
)

func daemonCommand(ctx context.Context, act string, input []byte, opt Options, stdout, stderr io.Writer) int {
	if opt.Locale == "" {
		opt.Locale = "en" // the offline Action catalog's default, independent of the console language
	}
	info, ok := launcher.ReadInfo(opt.DataRoot)
	if !ok {
		fmt.Fprintln(stderr, i18n.New().T(opt.Locale, "features.reason.unavailable"))
		return 1
	}
	token, err := web.ReadToken(opt.DataRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	client := launcher.NewClient(info.Bind, token)
	defer client.Close(context.Background())
	remote := &daemonExecutor{client: client, catalog: i18n.New()}
	if opt.Interactive && opt.Output == "rich" {
		if mode, ok := interactiveMode(act); ok {
			if err := tui.RunInteractive(ctx, remote, mode, opt.Locale, os.Stdin, stdout); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return 0
		}
	}
	res, err := client.Execute(ctx, action.Request{Action: act, Input: input, Locale: opt.Locale, RequestID: "cli"})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return WriteResult(stdout, opt, res)
}

// These flags name files in the invoking shell, whereas attachments and backup names are relative
// to the data root. Resolve shell paths before handing the action to a daemon with a different cwd.
func commandPaths(act string, input json.RawMessage) (json.RawMessage, error) {
	field := ""
	switch {
	case strings.HasPrefix(act, "config."):
		field = "file"
	case act == "imports.start" || act == "imports.preview" || act == "migration.apply":
		field = "path"
	default:
		return input, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return nil, err
	}
	var path string
	if json.Unmarshal(fields[field], &path) != nil || path == "" || filepath.IsAbs(path) {
		return input, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fields[field], _ = json.Marshal(abs)
	return json.Marshal(fields)
}

type daemonExecutor struct {
	client  *launcher.Client
	catalog *i18n.Catalog
}

func (d *daemonExecutor) Execute(ctx context.Context, req action.Request) action.Result {
	res, err := d.client.Execute(ctx, req)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	return res
}

func (d *daemonExecutor) Msg(locale, key string, args ...any) string {
	return d.catalog.T(locale, key, args...)
}

func (d *daemonExecutor) HasMessage(key string) bool { return d.catalog.Has(key) }

// Interactive screens refresh from live Actions without creating a local Service or database pool.
func (d *daemonExecutor) Subscribe() (<-chan application.Event, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan application.Event)
	go func() {
		defer close(ch)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, kind := range []string{"job", "config"} {
					select {
					case ch <- application.Event{Kind: kind}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return ch, cancel
}
