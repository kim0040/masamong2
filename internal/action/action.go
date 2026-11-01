// Package action is the wire-level Command/Query envelope shared by CLI, GUI, and the Discord simulator.
// Transports must not call storage directly (02장, 17장).
package action

import (
	"encoding/json"
	"fmt"
)

type Request struct {
	RequestID      string          `json:"request_id"`
	Action         string          `json:"action"`
	ResourceID     string          `json:"resource_id,omitempty"`
	BaseRevision   int             `json:"base_revision,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	Locale         string          `json:"locale,omitempty"`
	// Invoker is set only by in-process callers (Discord connector, chat turn) and never decoded from
	// transport JSON, so a client cannot claim to be someone else. nil = operator (CLI/GUI).
	Invoker *Invoker `json:"-"`
	// Operator is the signed-in console admin (set by the web transport, never decoded from JSON).
	// nil = the installation owner (CLI, one-time link, token).
	Operator *Operator `json:"-"`
}

// Operator identifies a console admin for preferences, permissions and the change log.
type Operator struct {
	ID   string
	Name string
	Role string // owner | operator | viewer
}

// Invoker is who triggered a request from a chat surface. Via is chat | command | test.
type Invoker struct {
	Via       string
	GuildID   string
	ChannelID string
	UserID    string
	// Manager is set by the Discord connector from Discord's own permissions: the invoker owns the
	// server or has Manage Server / Administrator there.
	Manager bool
	// Private is set by the connector when only the invoker sees the answer: a direct message, or a
	// slash command answered ephemerally. Personal results (a fortune) are shown in full only then.
	Private bool
	// Name is the invoker's display name (a fortune greets them by it).
	Name string
}

type Result struct {
	OK        bool           `json:"ok"`
	Action    string         `json:"action"`
	Revision  int            `json:"revision,omitempty"`
	JobID     string         `json:"job_id,omitempty"`
	Output    map[string]any `json:"output,omitempty"`
	Error     *Error         `json:"error,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
}

type Error struct {
	Code       string         `json:"code"`
	MessageKey string         `json:"message_key"`
	Message    string         `json:"message"`
	Args       map[string]any `json:"args,omitempty"`
	Retryable  bool           `json:"retryable"`
	RequestID  string         `json:"request_id,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func Fail(req Request, code, key, msg string, args map[string]any) Result {
	return Result{
		OK:        false,
		Action:    req.Action,
		RequestID: req.RequestID,
		Error: &Error{
			Code:       code,
			MessageKey: key,
			Message:    msg,
			Args:       args,
			RequestID:  req.RequestID,
		},
	}
}

func OK(req Request, out map[string]any) Result {
	return Result{OK: true, Action: req.Action, RequestID: req.RequestID, Output: out}
}
