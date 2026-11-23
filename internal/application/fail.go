package application

import (
	"context"
	"log/slog"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// failErr is the Action failure for an internal error. The person gets one plain sentence for key, in
// the request's language (one key per kind of failure: locales/*.json "error.*" and the feature keys);
// the Go error goes to the log with the Action's name and never into the result, so no library or
// database text reaches the lite page, the TUI or the CLI. msgArgs fill {placeholders} of the key.
func (s *Service) failErr(req action.Request, code, key string, err error, args map[string]any, msgArgs ...any) action.Result {
	level := slog.LevelError
	switch code {
	case errcode.InvalidInput, errcode.PermissionDenied, errcode.NotSetup, errcode.StaleRevision, errcode.FeatureDisabled,
		errcode.ConfigInvalid, errcode.ConfigConflict, errcode.ProviderUnconfigured:
		level = slog.LevelWarn // the caller or the setup is at fault, not the machine
	}
	s.Log.Log(context.Background(), level, "action failed", "action", req.Action, "code", code, "key", key, "err", err)
	return action.Fail(req, code, key, s.msg(req, key, msgArgs...), args)
}
