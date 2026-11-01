package conversation

import (
	"slices"
	"sort"
	"time"
)

// Options tune attribution. Zero values take the defaults below (docs/plan-memory-conversation.md §5.1).
type Options struct {
	BotID string
	// Names are the people of the channel (NamesFrom); a line that calls someone else by name is
	// directed at them.
	Names Names
	// BurstGap joins one person's messages sent close together (default 20 s).
	BurstGap time.Duration
	// FollowGap is how long after the bot answered someone their next undirected message still counts
	// as part of that exchange (default 3 min).
	FollowGap time.Duration
	// Window is how far back the caller's conversation is followed (default 30 min).
	Window time.Duration
	// LeadIn is how far before the conversation started other lines are offered as Candidates the call
	// may point at (default 3 min); LeadInMax caps them (default 8).
	LeadIn    time.Duration
	LeadInMax int
}

func (o Options) withDefaults() Options {
	if o.BurstGap <= 0 {
		o.BurstGap = 20 * time.Second
	}
	if o.FollowGap <= 0 {
		o.FollowGap = 3 * time.Minute
	}
	if o.Window <= 0 {
		o.Window = 30 * time.Minute
	}
	if o.LeadIn <= 0 {
		o.LeadIn = 3 * time.Minute
	}
	if o.LeadInMax <= 0 {
		o.LeadInMax = 8
	}
	return o
}

// Attribution is the caller's conversation inside the channel.
type Attribution struct {
	// Included are the IDs of the lines that belong to the caller's conversation, oldest first (the call
	// itself included).
	Included []string
	// Reasons says why each included line is in (call, reply, replied_to, bot_reply, to_bot, follow,
	// burst, to_caller) — recorded in the turn ledger so a wrong answer can be traced.
	Reasons map[string]string
	// Candidates are recent lines from other conversations the call might still point at ("what about
	// that one?"). They stay out of the prompt unless a classifier picks them (fail-closed).
	Candidates []string
	// Excluded counts the lines inside the window that were left out.
	Excluded int
}

// Attribute finds the conversation that the call (one message, or several the merger joined) belongs to.
// lines may be in any order; lines after the last call message are ignored.
func Attribute(lines []Line, callIDs []string, callerID string, opt Options) Attribution {
	opt = opt.withDefaults()
	sorted := append([]Line(nil), lines...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	byID := make(map[string]int, len(sorted))
	for i, l := range sorted {
		byID[l.ID] = i
	}
	out := Attribution{Reasons: map[string]string{}}
	in := map[string]bool{}
	last := -1
	var firstCall time.Time
	for _, id := range callIDs {
		i, ok := byID[id]
		if !ok {
			continue
		}
		in[id] = true
		out.Reasons[id] = "call"
		if i > last {
			last = i
		}
		if firstCall.IsZero() || sorted[i].At.Before(firstCall) {
			firstCall = sorted[i].At
		}
	}
	if last < 0 {
		return out
	}
	lastAt := sorted[last].At
	from := 0
	for from < last && lastAt.Sub(sorted[from].At) > opt.Window {
		from++
	}
	window := sorted[from : last+1]
	authorOf := func(id string) (string, bool, bool) {
		if i, ok := byID[id]; ok {
			return sorted[i].AuthorID, sorted[i].Bot, true
		}
		return "", false, false
	}
	// directedElsewhere: the line speaks to someone who is neither its author, the caller nor the bot —
	// a reply to them, a mention, a role or @everyone, or their name.
	directedElsewhere := func(l Line) bool {
		if l.ReplyTo != "" {
			if a, bot, ok := authorOf(l.ReplyTo); ok && !bot && a != l.AuthorID && a != callerID {
				return true
			}
		}
		if l.MentionsOthers {
			return true
		}
		for _, m := range l.Mentions {
			if m != opt.BotID && m != l.AuthorID && m != callerID {
				return true
			}
		}
		for _, id := range opt.Names.Called(l.Text) {
			if id != l.AuthorID && id != callerID {
				return true
			}
		}
		return false
	}
	toBot := func(l Line) bool {
		if l.mentions(opt.BotID) {
			return true
		}
		if l.ReplyTo != "" {
			if _, bot, ok := authorOf(l.ReplyTo); ok && bot {
				return true
			}
		}
		return false
	}
	// another caller: someone else speaking to the bot starts their own conversation, which stays out
	// unless it links into this one by a reply
	otherCall := func(l Line) bool { return !l.Bot && l.AuthorID != callerID && toBot(l) }
	add := func(id, why string) bool {
		if in[id] {
			return false
		}
		in[id] = true
		out.Reasons[id] = why
		return true
	}
	for changed := true; changed; {
		changed = false
		for i, l := range window {
			if l.ID == "" {
				continue
			}
			if in[l.ID] {
				// what an included line replies to is part of the conversation
				if l.ReplyTo != "" && !in[l.ReplyTo] {
					if j, ok := byID[l.ReplyTo]; ok && j >= from && j <= last {
						changed = add(l.ReplyTo, "replied_to") || changed
					}
				}
				continue
			}
			switch {
			case l.ReplyTo != "" && in[l.ReplyTo]:
				// a reply into the conversation, also from someone else asking the bot about the same thing
				changed = add(l.ID, "reply") || changed
			case l.Bot && l.ReplyTo != "":
				// the bot's answer to the caller (or to a line already in the conversation)
				if a, _, ok := authorOf(l.ReplyTo); (ok && a == callerID) || in[l.ReplyTo] {
					changed = add(l.ID, "bot_reply") || changed
					if !in[l.ReplyTo] {
						if _, ok := byID[l.ReplyTo]; ok {
							add(l.ReplyTo, "to_bot")
						}
					}
				}
			case !l.Bot && l.AuthorID == callerID && toBot(l) && !directedElsewhere(l):
				changed = add(l.ID, "to_bot") || changed
			case !l.Bot && l.AuthorID == callerID && !directedElsewhere(l) && followsBot(window, i, callerID, in, opt.FollowGap):
				changed = add(l.ID, "follow") || changed
			case !l.Bot && burstOf(window, i, in, opt.BurstGap) && !directedElsewhere(l) && !otherCall(l):
				changed = add(l.ID, "burst") || changed
			case !l.Bot && l.AuthorID != callerID && l.mentions(callerID) && !otherCall(l):
				changed = add(l.ID, "to_caller") || changed
			}
		}
	}
	start := lastAt
	for _, l := range window {
		if in[l.ID] {
			if l.At.Before(start) {
				start = l.At
			}
			out.Included = append(out.Included, l.ID)
		} else {
			out.Excluded++
		}
	}
	// candidates: other people's recent lines from the lead-in before the conversation started up to the
	// call, newest kept when there are too many
	var cands []string
	for _, l := range window {
		if in[l.ID] || l.Bot || l.ID == "" || l.Text == "" {
			continue
		}
		if l.At.Before(start.Add(-opt.LeadIn)) {
			continue
		}
		cands = append(cands, l.ID)
	}
	if len(cands) > opt.LeadInMax {
		cands = cands[len(cands)-opt.LeadInMax:]
	}
	out.Candidates = cands
	return out
}

// followsBot reports whether the line at i comes soon after the bot answered the caller, with no
// conversation-changing line of the caller in between.
func followsBot(window []Line, i int, callerID string, in map[string]bool, gap time.Duration) bool {
	for j := i - 1; j >= 0; j-- {
		p := window[j]
		if window[i].At.Sub(p.At) > gap {
			return false
		}
		if p.Bot && in[p.ID] {
			return true
		}
	}
	return false
}

// burstOf reports whether the line at i is one of a burst: the same author wrote an included line within
// gap of it, with nobody else speaking in between.
func burstOf(window []Line, i int, in map[string]bool, gap time.Duration) bool {
	l := window[i]
	for _, dir := range []int{-1, 1} {
		prev := l
		for j := i + dir; j >= 0 && j < len(window); j += dir {
			p := window[j]
			d := p.At.Sub(prev.At)
			if d < 0 {
				d = -d
			}
			if d > gap || p.AuthorID != l.AuthorID || p.Bot {
				break
			}
			if in[p.ID] {
				return true
			}
			prev = p
		}
	}
	return false
}

// Keep returns the lines whose IDs are listed, in time order.
func Keep(lines []Line, ids []string) []Line {
	var out []Line
	for _, l := range lines {
		if slices.Contains(ids, l.ID) {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
