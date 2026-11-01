package discord

import (
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

// People split one request over several messages: "마사몽" then "내일 날씨 어때?", or "내일" then
// "날씨 어때". Answered one by one, a mention-only room answers the bare call ("응?") and ignores the
// question, which has no mention; a room that answers everything answers each fragment. The merger
// holds an addressed message briefly and answers the person's messages as one turn:
//
//   - after a bare call (nothing but the bot's name or mention) it waits up to mergeCallWait for the
//     question; a message from the same person in the same channel joins the call even without a
//     mention;
//   - after a message with content it waits mergeSettle for more fragments;
//   - every new fragment restarts the wait, but no turn is held past mergeMax from its first message.
//
// Only the same person's messages in the same channel join; anyone else's are left alone. The idea of
// claiming the waiting input into the next step comes from DeepSeek Harness's agent inbox.
var (
	mergeSettle   = 1500 * time.Millisecond
	mergeCallWait = 10 * time.Second
	mergeMax      = 20 * time.Second
)

type pendingTurn struct {
	msgs       []*discordgo.MessageCreate
	bare       bool // every message so far is a bare call
	first      time.Time
	timer      *time.Timer
	generation uint64
}

type pendingMessage struct {
	key   string
	turn  *pendingTurn
	index int
}

type merger struct {
	mu      sync.Mutex
	pending map[string]*pendingTurn
	byID    map[string]pendingMessage
	fire    func(msgs []*discordgo.MessageCreate) // runs once per turn, off the lock
	forget  func(messageID string)                // a held message left without firing (deleted, or its turn stopped); off the lock
}

func mergeKey(m *discordgo.MessageCreate) string {
	return m.GuildID + "/" + m.ChannelID + "/" + m.Author.ID
}

// waiting reports whether this person has a turn being held in this channel.
func (mg *merger) waiting(m *discordgo.MessageCreate) bool {
	if m == nil || m.Author == nil {
		return false
	}
	mg.mu.Lock()
	defer mg.mu.Unlock()
	_, ok := mg.pending[mergeKey(m)]
	return ok
}

// add holds an addressed message, or a follow-up of a held turn; bare marks a message that only calls
// the bot.
func (mg *merger) add(m *discordgo.MessageCreate, bare bool) {
	if m == nil || m.Message == nil || m.Author == nil {
		return
	}
	key := mergeKey(m)
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if mg.pending == nil {
		mg.pending = map[string]*pendingTurn{}
	}
	p := mg.pending[key]
	if p == nil {
		p = &pendingTurn{bare: true, first: time.Now()}
		mg.pending[key] = p
	} else if p.timer != nil {
		p.timer.Stop()
	}
	p.msgs = append(p.msgs, m)
	if m.ID != "" {
		if mg.byID == nil {
			mg.byID = map[string]pendingMessage{}
		}
		mg.byID[m.ID] = pendingMessage{key: key, turn: p, index: len(p.msgs) - 1}
	}
	p.bare = p.bare && bare
	mg.scheduleLocked(key, p)
}

// edit replaces only the content field Discord reports for a pending message. A new message value keeps
// the other fields intact and avoids mutating a slice that a concurrent flush may already own.
func (mg *merger) edit(messageID, content string) bool {
	if messageID == "" {
		return false
	}
	mg.mu.Lock()
	defer mg.mu.Unlock()
	ref, ok := mg.byID[messageID]
	if !ok || mg.pending[ref.key] != ref.turn || ref.index >= len(ref.turn.msgs) {
		return false
	}
	old := ref.turn.msgs[ref.index]
	if old == nil || old.Message == nil {
		return false
	}
	clone := *old
	message := *old.Message
	message.Content = content
	clone.Message = &message
	ref.turn.msgs[ref.index] = &clone
	// The edit is new input, so a bare-call timeout should not keep the corrected text waiting.
	ref.turn.bare = false
	mg.scheduleLocked(ref.key, ref.turn)
	return true
}

// remove drops one pending message. Removing the final piece cancels the whole request without firing it.
func (mg *merger) remove(messageID string) bool {
	if messageID == "" {
		return false
	}
	mg.mu.Lock()
	ref, ok := mg.byID[messageID]
	if !ok || mg.pending[ref.key] != ref.turn || ref.index >= len(ref.turn.msgs) {
		mg.mu.Unlock()
		return false
	}
	defer mg.forgetIDs(messageID) // runs after the lock is released
	defer mg.mu.Unlock()
	p := ref.turn
	delete(mg.byID, messageID)
	p.msgs = append(p.msgs[:ref.index], p.msgs[ref.index+1:]...)
	for i := ref.index; i < len(p.msgs); i++ {
		if p.msgs[i] != nil && p.msgs[i].ID != "" {
			mg.byID[p.msgs[i].ID] = pendingMessage{key: ref.key, turn: p, index: i}
		}
	}
	if len(p.msgs) == 0 {
		mg.discardLocked(ref.key, p)
		return true
	}
	p.bare = false
	mg.scheduleLocked(ref.key, p)
	return true
}

// cancel drops every buffered piece for this person and channel.
func (mg *merger) cancel(m *discordgo.MessageCreate) bool {
	if m == nil || m.Author == nil {
		return false
	}
	key := mergeKey(m)
	mg.mu.Lock()
	p := mg.pending[key]
	if p == nil {
		mg.mu.Unlock()
		return false
	}
	var ids []string
	for _, held := range p.msgs {
		if held != nil && held.ID != "" {
			ids = append(ids, held.ID)
		}
	}
	mg.discardLocked(key, p)
	mg.mu.Unlock()
	mg.forgetIDs(ids...)
	return true
}

// forgetIDs tells the owner that held messages will never reach fire, so state kept for them can go.
func (mg *merger) forgetIDs(ids ...string) {
	if mg.forget == nil {
		return
	}
	for _, id := range ids {
		mg.forget(id)
	}
}

func (mg *merger) scheduleLocked(key string, p *pendingTurn) {
	if p.timer != nil {
		p.timer.Stop()
	}
	wait := mergeSettle
	if p.bare {
		wait = mergeCallWait
	}
	if left := time.Until(p.first.Add(mergeMax)); left < wait {
		wait = max(left, 0)
	}
	p.generation++
	generation := p.generation
	p.timer = time.AfterFunc(wait, func() { mg.flush(key, p, generation) })
}

func (mg *merger) discardLocked(key string, p *pendingTurn) {
	if mg.pending[key] == p {
		delete(mg.pending, key)
	}
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.generation++
	for _, m := range p.msgs {
		if m != nil && m.ID != "" {
			if ref, ok := mg.byID[m.ID]; ok && ref.turn == p {
				delete(mg.byID, m.ID)
			}
		}
	}
	p.msgs = nil
}

func (mg *merger) flush(key string, p *pendingTurn, generation uint64) {
	mg.mu.Lock()
	if mg.pending[key] != p || p.generation != generation {
		mg.mu.Unlock()
		return // a newer fragment restarted the wait
	}
	msgs := append([]*discordgo.MessageCreate(nil), p.msgs...)
	mg.discardLocked(key, p)
	mg.mu.Unlock()
	if mg.fire != nil && len(msgs) > 0 {
		mg.fire(msgs)
	}
}

// mergedText joins the held messages into one request, in order.
func mergedText(msgs []*discordgo.MessageCreate) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if c := bodyOf(m.Message); c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "\n")
}

// bareCall reports a message that only calls the bot: once its mentions and names are removed, at most
// one letter is left ("마사몽아", "야 마사몽~", "@마사몽 ?"). "마사몽 뭐해" is a question, not a call.
func bareCall(content, botID string, names []string) bool {
	rest := strings.ToLower(content)
	if botID != "" {
		rest = strings.NewReplacer("<@"+botID+">", " ", "<@!"+botID+">", " ").Replace(rest)
	}
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			rest = strings.ReplaceAll(rest, n, " ")
		}
	}
	if rest == strings.ToLower(content) {
		return false // no call in it at all
	}
	letters := 0
	for _, r := range rest {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letters++
		}
	}
	return letters <= 1
}
