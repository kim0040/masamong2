package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/memory/engine"
)

// harness drives one in-process Service the way masamongd's transports do: the Discord gateway
// (capture + chat turn per message, commands, poll buttons), the console (read-heavy Actions) and an
// operator changing settings, all at once.
type harness struct {
	svc *application.Service
	opt options
	rec *recorder

	turnsDone atomic.Int64
	inflight  sync.WaitGroup
	msgSeq    atomic.Int64

	mu     sync.Mutex
	polls  []string
	errors map[string]int // distinct error texts (first ones), for the report
}

func (h *harness) channel(i int) string { return fmt.Sprintf("ch%03d", i) }

var (
	subjects = []string{"캠핑", "점심", "시험", "여행", "게임", "영화", "회식", "이사", "생일", "운동", "축구", "주식"}
	verbs    = []string{"어디서 하기로 했지", "누가 준비한다고 했어", "언제였더라", "어떻게 됐어", "기억나?", "다시 얘기해 보자"}
	names    = []string{"민수", "지영", "현우", "수진", "도윤", "하은", "준호", "서연"}
	// polls.create uses a plain option label as its ID
	pollOptions = []string{"김밥", "라면", "국밥"}
)

// message is a chat line; the number makes most texts unique, as real traffic is (the query
// embedding cache sees realistic misses).
func (h *harness) message() string {
	n := h.msgSeq.Add(1)
	return fmt.Sprintf("마사몽 %s %s %s %d", names[rand.IntN(len(names))], subjects[rand.IntN(len(subjects))], verbs[rand.IntN(len(verbs))], n)
}

// arrivals sends Discord-like messages as a Poisson process until ctx ends.
func (h *harness) arrivals(ctx context.Context) {
	if h.opt.rate <= 0 {
		return
	}
	for {
		wait := time.Duration(rand.ExpFloat64() / h.opt.rate * float64(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		h.inflight.Add(1)
		go func() {
			defer h.inflight.Done()
			h.onMessage()
		}()
	}
}

func (h *harness) onMessage() {
	chi := rand.IntN(h.opt.channels)
	ch := h.channel(chi)
	user := fmt.Sprintf("u%03d", rand.IntN(h.opt.users))
	text := h.message()
	if h.opt.memory {
		start := time.Now()
		eng, err := h.svc.MemoryEngine()
		if err == nil {
			_, err = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: ch, MessageID: fmt.Sprintf("m%d", h.msgSeq.Load()),
				AuthorID: user, AuthorName: user, Content: text, At: time.Now()})
		}
		h.rec.add("capture", h.outcome(err), time.Since(start))
	}
	reply := rand.Float64() < h.opt.reply
	if h.opt.features {
		// the gateway shows every channel message to participation; a called message is the direct path's
		start := time.Now()
		h.svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: ch, MessageID: fmt.Sprintf("m%d", h.msgSeq.Load()), AuthorID: user,
			AuthorName: user, Content: text, At: time.Now(), Addressed: reply || !participates(chi)})
		h.rec.add("observe", "ok", time.Since(start))
	}
	if !reply {
		return
	}
	// the gateway gives a reply 120s, queueing included
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	start := time.Now()
	_, err := h.svc.ChatTurn(ctx, application.ChatInput{GuildID: "g", ChannelID: ch, Message: text, Speaker: user, SpeakerID: user})
	h.rec.add("chat_turn", h.outcome(err), time.Since(start))
	h.turnsDone.Add(1)
}

func (h *harness) outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, application.ErrChatBusy):
		return "busy"
	case errors.Is(err, application.ErrChatLimited):
		return "limited"
	case errors.Is(err, application.ErrChatBudget):
		return "budget"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	msg := err.Error()
	if strings.Contains(msg, "status 502") {
		return "provider_error" // injected by -llm-error-rate
	}
	h.mu.Lock()
	if len(h.errors) < 20 {
		h.errors[clip(msg, 200)]++
	}
	h.mu.Unlock()
	return "error"
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (h *harness) exec(kind, act string, in map[string]any, requestID string, inv *action.Invoker) action.Result {
	raw, _ := json.Marshal(in)
	start := time.Now()
	res := h.svc.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: "ko", RequestID: requestID, Invoker: inv})
	out := "ok"
	if !res.OK {
		out = "refused"
		if res.Error != nil {
			out = "refused:" + res.Error.Code
		}
	}
	h.rec.add(kind, out, time.Since(start))
	return res
}

// every runs fn at rate per second (Poisson) until ctx ends.
func (h *harness) every(ctx context.Context, rate float64, fn func()) {
	if rate <= 0 {
		return
	}
	for {
		wait := time.Duration(rand.ExpFloat64() / rate * float64(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		h.inflight.Add(1)
		go func() {
			defer h.inflight.Done()
			fn()
		}()
	}
}

// command is a slash/prefix command or a poll button press, as the gateway sends them.
func (h *harness) command() {
	ch := h.channel(rand.IntN(h.opt.channels))
	user := fmt.Sprintf("u%03d", rand.IntN(h.opt.users))
	inv := &action.Invoker{Via: "command", GuildID: "g", ChannelID: ch, UserID: user}
	switch rand.IntN(5) {
	case 0:
		h.exec("cmd_help", "help.version", map[string]any{}, "dc:help", inv)
	case 1:
		res := h.exec("cmd_poll_create", "polls.create", map[string]any{"question": "점심 뭐 먹지?", "options": pollOptions,
			"scope": "discord:" + ch}, "dc:poll", inv)
		if res.OK {
			if id, ok := res.Output["id"].(string); ok {
				h.mu.Lock()
				h.polls = append(h.polls, id+"|"+ch)
				if len(h.polls) > 50 {
					h.polls = h.polls[1:]
				}
				h.mu.Unlock()
			}
		}
	case 2, 3:
		h.mu.Lock()
		if len(h.polls) == 0 {
			h.mu.Unlock()
			return
		}
		p := h.polls[rand.IntN(len(h.polls))]
		h.mu.Unlock()
		id, pch, _ := strings.Cut(p, "|")
		// a button press: applied to the current revision, like the gateway does
		h.exec("button_vote", "polls.vote", map[string]any{"id": id, "scope": "discord:" + pch, "participant": user,
			"option_ids": []string{pollOptions[rand.IntN(len(pollOptions))]}, "latest": true}, "dc:vote", nil)
	case 4:
		h.exec("cmd_tools", "tools.list", map[string]any{}, "dc:tools", inv)
	}
}

// console is what an operator's open browser tabs poll.
func (h *harness) console() {
	acts := []string{"dashboard.overview", "turns.list", "stats.series", "usage.budgets", "runtime.logs", "jobs.list",
		"memory.blocks.list", "audit.list", "runtime.status"}
	act := acts[rand.IntN(len(acts))]
	h.exec("console:"+act, act, map[string]any{"limit": 50}, "web", nil)
}

// configChurn changes a channel's rules now and then, as an operator tuning the bot would. Every change
// is a new config revision (and a rebuilt memory engine) while turns are running.
func (h *harness) configChurn(ctx context.Context) {
	if h.opt.configEvery <= 0 {
		return
	}
	t := time.NewTicker(h.opt.configEvery)
	defer t.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ch := h.channel(rand.IntN(h.opt.channels))
		h.exec("config_change", "channels.configure", map[string]any{"guild_id": "g", "id": ch, "rules": fmt.Sprintf("짧게 답해 (%d)", i)}, "web", nil)
	}
}
