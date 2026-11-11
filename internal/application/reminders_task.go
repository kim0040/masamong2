package application

import (
	"context"
	"errors"
	"fmt"
	"masamong/internal/errclass"
	"slices"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/lang"
)

// Scheduled tasks (docs/plan-discord-experience.md §2.4): "매일 오전 9시에 국제 정세 뉴스 정리해서
// 알려줘". A task is a reminder row of kind "task" whose Task is a request written to stand on its own;
// when it is due the bot runs it as a turn of its own (tool loop, read-only tools, the answer model) and
// posts the answer where it was asked. Everything else is the reminder's: repetition, pause, cancel,
// missed occurrences after downtime, quiet hours in DMs, the lease that sends an occurrence once.
//
// Asking is a conversation, not a form. The tool loop passes `ask` instead of `task` when something the
// run needs was not said (news about what?); nothing is saved, and the answer asks that in its own
// words. Once the request is whole and its time is not ambiguous, the task is set at once (no button):
// the answer confirms what the result says was set. Two possible times ("3시") still get the picker.
//
// A run (borrowed from Hermes' cron runs and OpenClaw's agentTurn jobs) has no chat history and no
// memory search, may use only the read-only tools in taskTools, is told it is a scheduled run that
// must not schedule anything, and is not kept in the channel's conversation (the same request every day
// would fill it). It is recorded as a turn, so the console shows what it did and spent.

const (
	reminderKindTask = "task"
	taskMaxRunes     = 600
	taskRunTimeout   = 150 * time.Second
	// taskMaxUSD caps the paid optional steps of one run (chat.Guard), under limits.per_request_budget_usd
	taskMaxUSD = 0.03
)

// taskTools are what a scheduled run may use: lookups that change nothing. Pictures, polls, reminders
// (a run never schedules), memory and the operator's own HTTP tools stay out.
var taskTools = []string{"search.web", "web.read", "places.search", "weather.query", "disasters.query",
	"finance.quote", "finance.fx", "activity.ranking"}

// taskProposalRefusal answers a task request that cannot be saved yet: a detail the run needs is missing
// (the loop passed `ask`, or nothing says what to do), or the person has too many tasks.
func (s *Service) taskProposalRefusal(req action.Request, in map[string]any, userID, task string) *action.Result {
	ask := strings.TrimSpace(strVal(in, "ask", ""))
	if ask != "" || (task == "" && strings.TrimSpace(strVal(in, "what", "")) == "") {
		// nothing is saved: the answer asks, and the next message completes the request
		res := action.OK(req, map[string]any{"status": "needs_detail", "kind": reminderKindTask, "question": clip(ask, 200), "scheduled": false})
		return &res
	}
	if n := s.activeTasks(userID); n >= s.toolDefault("reminders.propose", "max_tasks", 5) {
		res := action.WithCard(action.OK(req, map[string]any{"status": "too_many_tasks", "active": n}),
			action.Card{Title: s.msg(req, "reminders.title"), Text: s.msg(req, "reminders.too_many_tasks", "n", n), Tone: "warn", Ephemeral: true})
		return &res
	}
	return nil
}

func (s *Service) activeTasks(userID string) int {
	db, err := s.adminDB()
	if err != nil {
		return 0
	}
	var n int
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM reminders WHERE owner_id=? AND kind=? AND `+reminderLive, userID, reminderKindTask).Scan(&n)
	return n
}

// taskProposed sets a task saved as proposed: at once when its time is clear, else it waits for the
// person to pick one of the times on the card. A chat answer gets no card; it says what was set.
func (s *Service) taskProposed(req action.Request, r reminder, private bool, now time.Time) action.Result {
	if len(r.Candidates) != 1 {
		out := map[string]any{"status": reminderProposed, "kind": reminderKindTask, "reminder": r, "needs": "pick_time"}
		return action.WithCard(action.OK(req, out), s.proposalCard(req, r, private))
	}
	dest := "channel"
	if private || r.GuildID == "" || r.ChannelID == "" {
		dest = "dm" // asked in DMs: it goes back there
	}
	set, first, refused := s.setReminder(req, r, dest, now)
	if refused != nil {
		s.cancelReminder(r) // the proposal is not left waiting for a button nobody was shown
		return *refused
	}
	loc := s.reminderLoc(set)
	where := s.msg(req, "reminders.dest.dm")
	if dest == "channel" {
		where = s.msg(req, "reminders.dest.channel")
	}
	out := map[string]any{"status": reminderScheduled, "kind": reminderKindTask, "id": set.ID, "name": set.Content, "task": set.Task,
		"first_run": s.whenText(req.Locale, first, loc), "where": where, "scheduled": true}
	fields := []action.CardField{{Name: s.msg(req, "reminders.field.when"), Value: out["first_run"].(string), Inline: true},
		{Name: s.msg(req, "reminders.field.where"), Value: where, Inline: true}}
	if set.Repeat != nil {
		out["repeat"] = s.repeatText(req.Locale, *set.Repeat, set)
		fields = append(fields, action.CardField{Name: s.msg(req, "reminders.field.repeat"), Value: out["repeat"].(string), Inline: true})
	} else {
		out["repeat"] = "none"
	}
	res := action.OK(req, out)
	if req.Invoker != nil && req.Invoker.Via == "chat" {
		return res // the answer confirms it in words
	}
	return action.WithCard(res, action.Card{Title: s.msg(req, "reminders.task_set_title"), Text: set.Task, Ephemeral: true, Fields: fields,
		Buttons: []action.CardButton{{ID: "remind:cancel:" + set.ID, Label: s.msg(req, "reminders.button.cancel"), Style: "danger"}}})
}

// errTaskHeld is an occurrence the bot may not run now (replies paused or off here, the monthly budget
// spent, DMs closed to chat): nothing is posted and a series goes on to its next occurrence.
var errTaskHeld = fmt.Errorf("%w: replies are held here", ErrNotDelivered)

// deliverTask runs a due task and posts its answer (or a short line saying it could not be done).
func (s *Service) deliverTask(ctx context.Context, r reminder, at time.Time, locale string, dm DiscordSender, poster ChannelPoster) (string, error) {
	if err := s.taskOpen(ctx, r); err != nil {
		s.Log.Info("reminders: scheduled task held", "reminder", r.ID, "reason", err)
		return "", errTaskHeld
	}
	req := action.Request{Locale: locale}
	loc := s.reminderLoc(r)
	runCtx, cancel := context.WithTimeout(ctx, taskRunTimeout)
	answer, runErr := s.runTask(runCtx, r, at, locale)
	cancel()
	if ctx.Err() != nil {
		return "", ctx.Err() // the daemon is stopping: this occurrence is left unknown, not half-posted
	}
	if runErr != nil || strings.TrimSpace(answer) == "" {
		s.Log.Warn("reminders: scheduled task produced no answer", "reminder", r.ID, "err", runErr)
		if r.Destination == "channel" {
			class := errclass.Of(runErr)
			if runErr == nil {
				class = errclass.Empty
			}
			s.LogProblem(r.GuildID, r.ChannelID, logTaskFailed, "", class)
		}
		answer = s.msg(req, "reminders.task_failed", "name", clip(r.Content, 80))
	}
	when := s.whenText(locale, at, loc)
	if r.Repeat != nil {
		when = s.repeatText(locale, *r.Repeat, r)
	}
	header := s.msg(req, "reminders.task_header", "name", clip(r.Content, 80), "when", when)
	sendCtx, cancelSend := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSend()
	switch {
	case r.Destination == "channel" && poster != nil:
		return poster.PostChannel(sendCtx, r.ChannelID, "", "-# "+header+"\n"+answer, nil)
	case r.Destination == "dm" && dm != nil:
		return "", dm.SendDM(sendCtx, r.OwnerID, action.Card{Title: header, Text: clip(answer, 3900), Tone: "reminder"}, "")
	}
	return "", ErrNotDelivered
}

// taskOpen is whether the bot may answer where the task posts: replies not paused or off there (a
// server channel), chat open to the person (DMs), the monthly budget not spent.
func (s *Service) taskOpen(ctx context.Context, r reminder) error {
	if r.GuildID == "" {
		if !s.DMChatAllowed(ctx, r.OwnerID) {
			return ErrDMClosedChat
		}
		if budget := parseUSD(s.Doc().Limits.MonthlyBudgetUSD, 0); budget > 0 {
			if _, spent, ok := s.monthSpend(); ok && spent >= budget {
				return ErrChatBudget
			}
		}
		return nil
	}
	return s.chatOpen(ChatInput{GuildID: r.GuildID, ChannelID: r.ChannelID, SpeakerID: r.OwnerID})
}

// runTask answers a task's request as the bot would in its channel: the channel's persona and rules, the
// tool loop over taskTools, no history or memory. It records the run as a turn.
func (s *Service) runTask(ctx context.Context, r reminder, at time.Time, locale string) (string, error) {
	started := time.Now()
	doc := s.Doc()
	pol := s.channelPolicy(r.GuildID, r.ChannelID)
	voice := s.voiceFor(r.GuildID, pol)
	voice.Pack = lang.For(chat.DetectLanguage(r.Task), locale, voice.Pack.Tag)
	routes, err := s.chatRoutes(doc)
	if err != nil {
		return "", err
	}
	all := s.chatToolset(voice, r.GuildID, r.ChannelID)
	toolset := chat.Toolset{Known: all.Known}
	for _, t := range all.Enabled {
		if slices.Contains(taskTools, t.ID) {
			toolset.Enabled = append(toolset.Enabled, t)
		}
	}
	in := ChatInput{GuildID: r.GuildID, ChannelID: r.ChannelID, SpeakerID: r.OwnerID, Speaker: r.OwnerName, Message: r.Task, turnID: newID("turn_")}
	persona := s.personaText(pol.PersonaRef)
	rules := strings.TrimSpace(pol.Rules + "\n\n" + lang.Fill(voice.Pack.Features.ScheduledTask,
		"name", r.OwnerName, "when", s.whenText(locale, at, s.reminderLoc(r))))
	maxUSD := taskMaxUSD
	if per := parseUSD(doc.Limits.PerRequestBudgetUSD, 0); per > 0 && per < maxUSD {
		maxUSD = per
	}
	rt := &chat.Runtime{LLM: chatLLM{s: s, routes: routes}, Tools: s.turnTool(in, voice, viaTask)} // no pages read (searchPages)
	res, err := rt.Answer(ctx, chat.Request{
		Message: r.Task, Speaker: r.OwnerName, Persona: persona, Rules: rules, AgentPrompt: doc.Memory.AgentPrompt,
		Tools: toolset, Voice: voice, PlannerModel: "planner", LoopModel: doc.Chat.LoopRole(), ResponseModel: "response",
		TZ: s.instanceZone(), BudgetUSD: parseUSD(doc.Memory.TurnBudgetUSD, 0.05),
		Guard: &chat.Guard{MaxUSD: maxUSD, MaxCalls: 3}, ToolMode: s.toolMode(), LoopFinal: doc.Chat.LoopFinalMode(),
	})
	if err == nil {
		res.Answer = s.safeAnswer(res.Answer, voice, chat.ComposeSystem(voice, persona, rules, doc.Memory.AgentPrompt))
	}
	outcome := "answered"
	switch {
	case errors.Is(err, context.Canceled):
		outcome = "canceled"
	case err != nil:
		outcome = "error"
	case strings.TrimSpace(res.Answer) == "":
		outcome = "silent"
	}
	names := []string{}
	for _, t := range res.Tools {
		names = append(names, t.Tool)
	}
	body := map[string]any{"scheduled": map[string]any{"reminder_id": r.ID, "occurrence": at.UTC().Format(time.RFC3339)},
		"message": clip(r.Task, 400), "speaker": r.OwnerName, "answer_preview": clip(res.Answer, 280), "answer": clip(res.Answer, 4000),
		"tools": recordTools(res.Tools), "timing_ms": res.Timing, "intent_source": res.Intent.Source}
	if err != nil {
		body["error"] = err.Error()
	}
	addToolLoop(body, res)
	body = s.withLoggingLevel(pol.Logging.Level, outcome, err, body)
	s.recordTurn(TurnRecord{ID: in.turnID, GuildID: r.GuildID, ChannelID: r.ChannelID, SpeakerID: r.OwnerID, Outcome: outcome, Tools: names,
		CostUSD: res.CostUSD, LatencyMS: int(time.Since(started).Milliseconds()), Body: body})
	return res.Answer, err
}

// reminderBriefs is a person's reminders as a chat answer reads them: what, when next, how often.
func (s *Service) reminderBriefs(req action.Request, list []reminder) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, r := range list {
		b := map[string]any{"id": r.ID, "name": clip(r.Content, 120), "status": r.Status}
		if r.Kind == reminderKindTask {
			b["kind"], b["task"] = reminderKindTask, clip(r.Task, 200)
		}
		if at, err := time.Parse(time.RFC3339, r.DueAt); err == nil && r.Status != reminderPaused {
			b["next"] = s.whenText(req.Locale, at, s.reminderLoc(r))
		}
		if r.Repeat != nil {
			b["repeat"] = s.repeatText(req.Locale, *r.Repeat, r)
		}
		out = append(out, b)
	}
	return out
}

// reminderByWords finds the one reminder of the person's that words point at ("뉴스", "약 먹기"): the
// one whose name or task holds most of the words. With no words, a person with one reminder means
// that one. Several equally good or none: nothing is changed, and the answer lists them to ask which.
func (s *Service) reminderByWords(req action.Request, userID, words string) (string, *action.Result) {
	list, err := s.listReminders(`owner_id=? AND `+reminderLive, []any{userID}, 25)
	if err != nil {
		res := s.myReminders(req, userID)
		return "", &res
	}
	best, top := []reminder{}, 0
	terms := strings.Fields(strings.ToLower(words))
	for _, r := range list {
		hay := strings.ToLower(r.Content + " " + r.Task + " " + r.Original)
		score := 0
		for _, t := range terms {
			if len([]rune(t)) >= 2 && strings.Contains(hay, t) {
				score++
			}
		}
		if len(terms) == 0 {
			score = 1
		}
		switch {
		case score > top:
			best, top = []reminder{r}, score
		case score == top && score > 0:
			best = append(best, r)
		}
	}
	if len(best) == 1 {
		return best[0].ID, nil
	}
	status := "ambiguous"
	if len(best) == 0 {
		status, best = "not_found", list
	}
	res := action.OK(req, map[string]any{"status": status, "changed": false, "reminders": s.reminderBriefs(req, best)})
	return "", &res
}
