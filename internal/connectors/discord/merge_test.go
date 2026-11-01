package discord

import (
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func msg(author, content string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{ID: content, GuildID: "g", ChannelID: "c", Content: content, Author: &discordgo.User{ID: author}}}
}

func shortWindows(t *testing.T) {
	old := [3]time.Duration{mergeSettle, mergeCallWait, mergeMax}
	mergeSettle, mergeCallWait, mergeMax = 40*time.Millisecond, 200*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { mergeSettle, mergeCallWait, mergeMax = old[0], old[1], old[2] })
}

type turns struct {
	mu  sync.Mutex
	got []string
	at  []time.Time
}

func (tr *turns) fire(msgs []*discordgo.MessageCreate) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.got = append(tr.got, mergedText(msgs))
	tr.at = append(tr.at, time.Now())
}

func (tr *turns) list() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.got...)
}

// "마사몽" then "내일 날씨 어때?": one turn with both, answered after the question, not the call.
func TestMergerJoinsACallWithItsQuestion(t *testing.T) {
	shortWindows(t)
	tr := &turns{}
	mg := &merger{fire: tr.fire}
	start := time.Now()
	mg.add(msg("a", "마사몽"), true)
	time.Sleep(100 * time.Millisecond) // longer than the settle window: a bare call waits longer
	if !mg.waiting(msg("a", "")) || len(tr.list()) != 0 {
		t.Fatal("a bare call is held for its question")
	}
	mg.add(msg("a", "내일 날씨 어때?"), false)
	time.Sleep(150 * time.Millisecond)
	if got := tr.list(); len(got) != 1 || got[0] != "마사몽\n내일 날씨 어때?" {
		t.Fatalf("turns: %q", got)
	}
	if d := tr.at[0].Sub(start); d < 100*time.Millisecond {
		t.Fatalf("answered too early: %v", d)
	}
}

// A call nobody follows is still answered; another person's messages never join.
func TestMergerAnswersALoneCallAndKeepsPeopleApart(t *testing.T) {
	shortWindows(t)
	tr := &turns{}
	mg := &merger{fire: tr.fire}
	mg.add(msg("a", "마사몽"), true)
	mg.add(msg("b", "마사몽 이거 알아?"), false)
	time.Sleep(300 * time.Millisecond)
	got := tr.list()
	if len(got) != 2 || got[0] != "마사몽 이거 알아?" || got[1] != "마사몽" {
		t.Fatalf("turns: %q", got)
	}
}

// Fragments keep restarting the wait, but a turn is never held past the cap.
func TestMergerCapsTheWait(t *testing.T) {
	shortWindows(t)
	tr := &turns{}
	mg := &merger{fire: tr.fire}
	start := time.Now()
	for i := 0; i < 20; i++ {
		mg.add(msg("a", "조각"), false)
		time.Sleep(25 * time.Millisecond)
		if len(tr.list()) > 0 {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if len(tr.at) == 0 {
		t.Fatal("no turn")
	}
	if d := tr.at[0].Sub(start); d > 480*time.Millisecond {
		t.Fatalf("held %v, past the cap", d)
	}
}

func TestBareCall(t *testing.T) {
	names := []string{"마사몽", "masamong"}
	for _, c := range []struct {
		content string
		bare    bool
	}{
		{"마사몽", true}, {"마사몽아", true}, {"야 마사몽~", true}, {"<@42> ?", true}, {"<@!42>", true}, {"Masamong!", true},
		{"마사몽 뭐해", false}, {"마사몽 내일 날씨 어때?", false}, {"<@42> 안녕", false}, {"오늘 덥다", false}, {"hey masamong", false},
	} {
		if got := bareCall(c.content, "42", names); got != c.bare {
			t.Errorf("bareCall(%q) = %v, want %v", c.content, got, c.bare)
		}
	}
}
