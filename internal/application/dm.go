package application

import (
	"context"
	"errors"
	"fmt"

	"masamong/internal/tools"
)

var ErrDMClosedChat = errors.New("chat: DM conversation is not open")

// DiscordMembership checks membership only in servers currently served by the bot.
type DiscordMembership interface {
	SharedGuildMember(context.Context, string) bool
}

func (s *Service) DMChatAllowed(ctx context.Context, userID string) bool {
	if userID == "" || s.BlockedIn("", userID) || s.Doc().DM.AllowMode() == "off" {
		return false
	}
	if s.IsOperator(userID) {
		return true
	}
	if s.Doc().DM.AllowMode() == "operators" {
		return false
	}
	gateway, ok := s.Discord.(DiscordMembership)
	return ok && gateway.SharedGuildMember(ctx, userID)
}

// DMLimited identifies which daily quota refused; each has its own notice.
type DMLimited struct{ Scope string }

func (e DMLimited) Error() string { return "chat: DM daily limit reached (" + e.Scope + ")" }

func (s *Service) reserveDM(in ChatInput) error {
	u := s.toolUsage()
	if u == nil {
		return ErrChatBusy
	}
	day, now := s.toolDay()
	dm := s.Doc().DM
	caller := tools.Caller{UserID: in.SpeakerID}
	reservations := []tools.Reservation{{Tool: "dm.chat", Limits: tools.Limits{PerUserPerDay: dm.UserDaily(), PerDay: dm.Daily()}, Caller: caller}}
	if r, ok := tools.Find(s.toolRegistry(), "chat.respond"); ok {
		reservations = append(reservations, tools.Reservation{Tool: r.ID, Limits: r.Limits,
			Caller: tools.Caller{UserID: in.SpeakerID, Channel: "discord::" + in.ChannelID}})
	}
	tool, dec, err := u.ReserveBatch(day, now, reservations)
	if err != nil {
		return ErrChatBusy
	}
	if dec.OK {
		return nil
	}
	if tool == "dm.chat" {
		return DMLimited{Scope: dec.Scope}
	}
	return fmt.Errorf("%w: %s", ErrChatLimited, dec.Scope)
}

// ClaimDMNotice persists the daily notice cap across gateway and service restarts.
func (s *Service) ClaimDMNotice(userID, kind string) bool {
	if s.BlockedIn("", userID) {
		return false
	}
	u := s.toolUsage()
	if u == nil {
		return false
	}
	day, now := s.toolDay()
	dec, err := u.Reserve(day, now, "dm.notice."+kind, tools.Limits{PerUserPerDay: 1}, tools.Caller{UserID: userID})
	return err == nil && dec.OK
}
