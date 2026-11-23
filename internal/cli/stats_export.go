package cli

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/i18n"
)

// `masamong stats export --from DATE --to DATE --file PATH` writes the weekly statistics file the
// stats.export Action builds. The Action returns the file in its output; --file is handled here, on the
// invoking side, so a command sent to a running daemon writes where the operator's shell is, not where the
// daemon's folder is. Nothing is sent anywhere. --reset-id replaces the random install id first.

// takeStatsFile moves --file out of the Action's input (it is not an Action field) into the options.
func takeStatsFile(kv map[string]any, opt *Options) error {
	v, ok := kv["file"]
	if !ok {
		return nil
	}
	path, isString := v.(string)
	if !isString || path == "" {
		return fmt.Errorf("--file needs a path")
	}
	opt.File = path
	delete(kv, "file")
	return nil
}

// writeStatsExportFile writes the exported file to path and returns the short result printed instead.
// A failure says "a file could not be read or written" in locale; the Go error goes to the log.
func writeStatsExportFile(path, locale string, res action.Result) action.Result {
	fail := func(err error) action.Result {
		slog.Warn("stats export file not written", "action", res.Action, "err", err)
		return action.Fail(action.Request{Action: res.Action, RequestID: res.RequestID}, errcode.IO, "error.file", i18n.New().T(locale, "error.file"), nil)
	}
	file, ok := res.Output["file"]
	if !ok {
		return fail(fmt.Errorf("the export has no file"))
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fail(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fail(err)
	}
	out := map[string]any{"written": path, "bytes": len(raw), "collecting": res.Output["collecting"]}
	if f, ok := file.(map[string]any); ok {
		out["period"] = f["period"]
	}
	res.Output = out
	return res
}
