// Package discord is the local interaction simulator (09). It never opens a Gateway or sends REST.
package discord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/catalog"
)

type Event struct {
	Kind      string         `json:"kind"` // slash|prefix|button|dm
	Command   string         `json:"command"`
	Action    string         `json:"action"`
	Actor     string         `json:"actor"`
	Token     string         `json:"token"`
	AckWithin time.Duration  `json:"-"`
	Input     map[string]any `json:"input"`
}

type SimResult struct {
	Acked           bool          `json:"acked"`
	Deferred        bool          `json:"deferred"`
	Action          string        `json:"action"`
	AllowedMentions []string      `json:"allowed_mentions"`
	Result          action.Result `json:"result"`
	Error           string        `json:"error,omitempty"`
}

type Simulator struct {
	Svc         *application.Service
	SenderLease string
	tokens      map[string]tokenMeta
}

type tokenMeta struct {
	Actor    string
	Expires  time.Time
	Revision int
}

func New(svc *application.Service, lease string) *Simulator {
	return &Simulator{Svc: svc, SenderLease: lease, tokens: map[string]tokenMeta{}}
}

func (s *Simulator) Handle(ctx context.Context, ev Event, now time.Time, ourLease string) SimResult {
	if ourLease != s.SenderLease {
		return SimResult{Error: "sender_lease_lost"}
	}
	if ev.Kind == "button" {
		meta, ok := s.tokens[ev.Token]
		if !ok || now.After(meta.Expires) || meta.Actor != ev.Actor {
			return SimResult{Acked: true, Error: "stale_or_foreign_token"}
		}
	}
	act := ev.Action
	if act == "" {
		act = mapLegacy(ev.Command)
	}
	if _, ok := catalog.Lookup(act); !ok {
		return SimResult{Acked: true, Error: "unknown_action"}
	}
	acked := ev.AckWithin == 0 || ev.AckWithin <= 3*time.Second
	raw, _ := json.Marshal(ev.Input)
	if ev.Input == nil {
		raw = nil
	}
	res := s.Svc.Execute(ctx, action.Request{Action: act, Input: raw, RequestID: "sim"})
	return SimResult{
		Acked: acked, Deferred: !acked && ev.AckWithin <= 15*time.Minute,
		Action: act, Result: res, AllowedMentions: nil,
	}
}

func (s *Simulator) IssueButton(actor string, now time.Time, rev int) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	tok := hex.EncodeToString(b[:])
	s.tokens[tok] = tokenMeta{Actor: actor, Expires: now.Add(15 * time.Minute), Revision: rev}
	return tok
}

func mapLegacy(cmd string) string {
	if c, ok := ByAlias(cmd); ok {
		return c.Action
	}
	return cmd
}
