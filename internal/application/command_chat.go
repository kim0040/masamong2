package application

import (
	"context"
	"strings"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/errcode"
	"masamong/internal/tools"
)

// unansweredStatuses are the statuses a tool's result uses for "it ran, but has nothing to tell": no
// key, nothing found, no area to search around, the service down. One list for every tool, so the
// command path and the answer prompt agree on what a failure is.
var unansweredStatuses = map[string]bool{"unconfigured": true, "no_results": true, "no_area": true, "unavailable": true, "not_found": true, "error": true}

// Unanswered reports whether a result gave the person nothing: it failed, or its status says so. A
// refusal (no permission) is an answer: the person is told and nothing else is tried.
func Unanswered(res action.Result) bool {
	if !res.OK {
		return res.Error == nil || (res.Error.Code != errcode.PermissionDenied && res.Error.Code != errcode.Unauthorized)
	}
	st, _ := res.Output["status"].(string)
	return unansweredStatuses[st]
}

// CommandToChat carries on a command that had no answer as a conversation. "!시세 삼성전자" without a
// quote service used to end on an error card; now, when the command ran a tool the chat may use and
// the channel answers people, the same message becomes a chat turn that starts from the result: the
// failed tool is not tried again, the planner may pick another (a web search), and the reply is a real
// one, not a canned notice. ok is false when the card stays: the command answered, it is not a chat
// tool, the channel does not chat, or the turn itself could not run (budget, limits, model down).
func (s *Service) CommandToChat(ctx context.Context, res action.Result, in ChatInput) (chat.Result, bool) {
	if !Unanswered(res) || strings.TrimSpace(in.Message) == "" {
		return chat.Result{}, false
	}
	if in.GuildID == "" && !s.DMChatAllowed(ctx, in.SpeakerID) {
		return chat.Result{}, false
	}
	t, found := tools.Find(s.toolRegistry(), res.Action)
	if !found || !t.CanChat {
		return chat.Result{}, false
	}
	if s.ChannelPolicyFor(in.GuildID, in.ChannelID).Respond == "off" {
		return chat.Result{}, false
	}
	in.Earlier = append(in.Earlier, chatView(res))
	out, err := s.ChatTurn(ctx, in)
	if err != nil || strings.TrimSpace(out.Answer) == "" {
		if err != nil {
			if in.OnRefusal != nil {
				in.OnRefusal(err)
			}
			s.Log.Debug("command: chat follow-up skipped", "action", res.Action, "err", err)
		}
		return chat.Result{}, false
	}
	return out, true
}
