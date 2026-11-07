package application

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/memory/store"
)

// chainOf writes n replies, each built on the turns before it, the first of them on source.
func chainOf(t *testing.T, s *Service, channel, source string, n int) []store.ChatTurn {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	key := "discord:g:" + channel
	sources := []string{source}
	for i := range n {
		native := fmt.Sprintf("%s-q%d", channel, i)
		if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
			{Role: "user", Speaker: "bob", Content: "question", NativeID: native, SpeakerID: "bob"},
			{Role: "assistant", Content: "answer", NativeID: native, SpeakerID: "bob", SourceIDs: sources},
		}, 40); err != nil {
			t.Fatal(err)
		}
		turns, err := e.Store.RecentChatTurns(key, 16)
		if err != nil {
			t.Fatal(err)
		}
		sources = nil
		for _, turn := range turns {
			sources = append(sources, turn.SourceIDs...)
		}
	}
	turns, err := e.Store.RecentChatTurns(key, 16)
	if err != nil {
		t.Fatal(err)
	}
	return turns
}

func repliesKept(t *testing.T, s *Service, channel string) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key=? AND role='assistant'`, "discord:g:"+channel).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A turn that read the window before the deletion and writes after the worker finished links to turns
// that are gone. It cannot be kept: nothing would ever find it again.
func TestAReplyWrittenAfterTheRedactionCannotLinkToARemovedTurn(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	window := chainOf(t, s, "c", "secret", 3)
	var read []string
	for _, turn := range window {
		read = append(read, turn.SourceIDs...)
	}
	if err := s.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatalf("replies after the redaction: %d", n)
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "assistant", Content: "late", NativeID: "late-q", SpeakerID: "bob", SourceIDs: read}}, 40); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatal("a late reply linked to removed turns was kept")
	}
	s.recordTurn(TurnRecord{ID: "late-turn", GuildID: "g", ChannelID: "c", SpeakerID: "bob", Body: map[string]any{
		"message": "late", "answer": "late answer built on them", "answer_preview": "late answer",
		"context": map[string]any{"observation_ids": read}}})
	if raw := turnBody(t, s, "late-turn"); strings.Contains(raw, "built on them") || !strings.Contains(raw, `"evidence_redacted":true`) {
		t.Fatal("a late turn record built on removed turns kept its answer: " + raw)
	}
	var indexed int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_kind=? AND holder_id='late-turn' AND source_id GLOB 'chat:*'`, holderTurnEvidence).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatalf("the refused turn stays indexed: %d %v", indexed, err)
	}
}

// A reply committed while the job waits in the queue links to turns the worker has not reached yet: the
// walk happens when the worker runs, so it is found.
func TestAReplyCommittedWhileTheJobWaitsIsFound(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	window := chainOf(t, s, "c", "secret", 3)
	var read []string
	for _, turn := range window {
		read = append(read, turn.SourceIDs...)
	}
	s.redaction.running.Store(true)
	if err := s.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "assistant", Content: "meanwhile", NativeID: "meanwhile-q", SpeakerID: "bob", SourceIDs: read}}, 40); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 4 {
		t.Fatalf("replies before the worker ran: %d, want 4", n)
	}
	s.redaction.running.Store(false)
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatalf("replies after the worker ran: %d", n)
	}
}

// The chat holders of a chain stay as long as anything still kept links through them, and go all at once,
// however deep, when nothing does.
func TestRetentionKeepsAChainWhileAReplyLinksThroughIt(t *testing.T) {
	s := privacyService(t)
	const depth = 60 // past the 40 kept turns
	chainOf(t, s, "c", "secret", depth)
	deadHolders := func() int {
		t.Helper()
		var n int
		if err := s.Store.DB.QueryRow(`SELECT COUNT(DISTINCT holder_id) FROM evidence_refs WHERE holder_kind='chat' AND CAST(substr(holder_id,6) AS INTEGER) NOT IN (SELECT id FROM chat_turns)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	dead := deadHolders()
	if dead == 0 {
		t.Fatal("no turn has left the kept window: nothing is tested")
	}
	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deadHolders(); got != dead {
		t.Fatalf("retention removed the index entries of %d trimmed turns the kept replies link through", dead-got)
	}
	var reachable int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE source_id='secret' AND holder_kind='chat'`).Scan(&reachable); err != nil || reachable != 1 {
		t.Fatalf("the first reply's entry for the source: %d %v", reachable, err)
	}

	if _, err := s.Store.DB.Exec(`DELETE FROM chat_turns`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := deadHolders(); got != 0 {
		t.Fatalf("%d chat holders of a chain nothing links to survived one retention pass", got)
	}
}

// A person who withdraws reaches replies built on their words through a turn of theirs that left the kept
// window, even when their message was never stored as memory.
func TestWithdrawalFollowsLinksThroughATrimmedTurn(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "user", Speaker: "alice", Content: "my words", NativeID: "alice-q", SpeakerID: "alice"}}, 40); err != nil {
		t.Fatal(err)
	}
	turns, err := e.Store.RecentChatTurns("discord:g:c", 1)
	if err != nil || len(turns) != 1 {
		t.Fatalf("%d %v", len(turns), err)
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "assistant", Content: "reply to alice", NativeID: "alice-q", SpeakerID: "alice", SourceIDs: turns[0].SourceIDs}}, 40); err != nil {
		t.Fatal(err)
	}
	window, _ := e.Store.RecentChatTurns("discord:g:c", 16)
	var read []string
	for _, turn := range window {
		read = append(read, turn.SourceIDs...)
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "assistant", Content: "bob's reply", NativeID: "bob-q", SpeakerID: "bob", SourceIDs: read}}, 40); err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "privacy.consent", Input: []byte(`{"user_id":"alice","scope":"memory","decision":"withdraw"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatalf("%d replies built on a withdrawn person's words survived", n)
	}
}

// Between the walk and the rows being deleted, the tombstones alone refuse a reply on the lineage.
func TestTheWalkTombstonesTheLineageBeforeTheRowsGo(t *testing.T) {
	s := privacyService(t)
	e, _ := s.MemoryEngine()
	window := chainOf(t, s, "c", "secret", 3)
	var read []string
	for _, turn := range window {
		read = append(read, turn.SourceIDs...)
	}
	keys := map[string]bool{"secret": true}
	if err := s.closeChatLineage(context.Background(), s.Store, keys); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 3 {
		t.Fatalf("the walk deleted rows: %d replies left", n)
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "assistant", Content: "late", NativeID: "late-q", SpeakerID: "bob", SourceIDs: read}}, 40); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 3 {
		t.Fatal("a reply on a tombstoned lineage was kept")
	}
}
