package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
	"masamong/internal/storage"
	"masamong/internal/tools"
)

func privacyService(t *testing.T) *Service {
	t.Helper()
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"security"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	return s
}

func TestPrivacyReachesActiveAndClosedPracticeCopies(t *testing.T) {
	for _, change := range []string{"delete", "withdraw", "edit"} {
		for _, active := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/active=%v", change, active), func(t *testing.T) {
				s := privacyService(t)
				p, err := OpenPractice(context.Background(), s.DataRoot, "en", slog.Default())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Close() })
				text := "private copied practice canary"
				for _, target := range []*Service{s, p} {
					e, err := target.MemoryEngine()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", AuthorName: "Alice", Content: text, At: time.Now()}); err != nil {
						t.Fatal(err)
					}
					target.recordTurn(TurnRecord{GuildID: "g", ChannelID: "c", SpeakerID: "bob", Lab: true, Body: map[string]any{"hits": hitBrief([]search.Hit{{DocID: "test", Excerpt: text, ObservationIDs: []string{corpus.DiscordLiveObservationID("discord:g:c", "m1")}}})}})
				}
				if !active {
					if err := p.Close(); err != nil {
						t.Fatal(err)
					}
				}
				switch change {
				case "delete":
					s.MessagesDeleted("g", "c", []string{"m1"})
				case "edit":
					s.MessageEdited("g", "c", "m1", "replacement text", time.Now())
				case "withdraw":
					r := s.Execute(context.Background(), action.Request{Action: "privacy.consent", Input: []byte(`{"user_id":"alice","scope":"memory","decision":"withdraw"}`)})
					if !r.OK {
						t.Fatal(r.Error)
					}
				}
				db := p.Store
				if !active {
					db, err = storage.Open(filepath.Join(PracticeRoot(s.DataRoot), "masamong.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
				}
				var content string
				if err := db.DB.QueryRow(`SELECT content FROM memory_observations WHERE native_message_id='m1'`).Scan(&content); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(content, text) {
					t.Fatal("practice source copy survived")
				}
				var n int
				if err := db.DB.QueryRow(`SELECT COUNT(*) FROM turn_log WHERE body LIKE '%private copied practice canary%'`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("practice turn copy survived: %d %v", n, err)
				}
			})
		}
	}
}

func TestCrossAuthorEvidenceRedaction(t *testing.T) {
	for _, change := range []string{"delete", "withdraw", "edit"} {
		t.Run(change, func(t *testing.T) {
			s := privacyService(t)
			e, err := s.MemoryEngine()
			if err != nil {
				t.Fatal(err)
			}
			text := "private canary with a quoted \"address\" and newline\nsecret"
			_, err = e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "alice", AuthorName: "Alice", Content: text, At: time.Now().Add(-time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			obs := corpus.DiscordLiveObservationID("discord:g:c", "m1")
			if _, err := e.Store.SyncLeaves("discord:g:c", e.Settings.Recipe, time.Now(), nil); err != nil {
				t.Fatal(err)
			}
			var leaf string
			if err := s.Store.DB.QueryRow(`SELECT leaf_id FROM memory_leaves LIMIT 1`).Scan(&leaf); err != nil {
				t.Fatal(err)
			}
			hits := []search.Hit{{Rank: 1, DocID: leaf, Room: "discord:g:c", Excerpt: text, Speakers: []string{"Alice"}, ObservationIDs: []string{obs}, VisibleObservationIDs: []string{obs}, LeafObservationIDs: []string{obs}}}
			s.recordTurn(TurnRecord{ID: "bob", GuildID: "g", ChannelID: "c", SpeakerID: "bob", Lab: true, Body: map[string]any{"message": "Bob's question", "answer": "Bot answer remains", "hits": hitBrief(hits), "tools": []chat.ToolResult{{Tool: "memory.search", OK: true, Data: map[string]any{"hits": hits}}}}})
			var raw string
			_ = s.Store.DB.QueryRow(`SELECT body FROM turn_log WHERE id='bob'`).Scan(&raw)
			if strings.Contains(raw, "private canary") || !strings.Contains(raw, obs) || !strings.Contains(raw, personRef("alice")) {
				t.Fatalf("expected references without text: %s", raw)
			}
			r := s.Execute(context.Background(), action.Request{Action: "turns.get", Input: []byte(`{"id":"bob"}`)})
			shown, _ := json.Marshal(r.Output)
			if !strings.Contains(string(shown), "private canary") {
				t.Fatalf("references did not resolve: %s", shown)
			}
			// Legacy rows had no observation IDs, only text or leaf IDs. Include tool data and lab copies.
			legacy, _ := json.Marshal(map[string]any{"message": "Bob question", "hits": []any{map[string]any{"doc_id": leaf, "excerpt": text}}, "tools": []any{map[string]any{"tool": "memory.search", "data": map[string]any{"excerpt": text}}}})
			if _, err := s.Store.DB.Exec(`INSERT INTO turn_log(id,at,day,hour,guild_id,channel_id,outcome,body) VALUES('legacy','now','today',0,'g','c','answered',?)`, string(legacy)); err != nil {
				t.Fatal(err)
			}
			meta, _ := json.Marshal(map[string]any{"doc_ids": []string{leaf}})
			if _, err := s.Store.DB.Exec(`INSERT INTO lab_reviews(id,at,channel_key,question,answer,verdict,tags,note,meta) VALUES('review','now','g:c','q',?,'good','','',?)`, text, string(meta)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Store.DB.Exec(`INSERT INTO memory_reviews(leaf_id,room,kind,excerpt,at) VALUES(?,'discord:g:c','correct',?,'now')`, leaf, text); err != nil {
				t.Fatal(err)
			}
			items, _ := json.Marshal([]any{map[string]any{"review_id": "review", "question": text, "doc_ids": []string{leaf}}})
			if _, err := s.Store.DB.Exec(`INSERT INTO eval_runs(id,at,params,summary,items) VALUES('eval','now','{}','{}',?)`, string(items)); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "delete":
				s.MessagesDeleted("g", "c", []string{"m1"})
			case "edit":
				s.MessageEdited("g", "c", "m1", "revised text", time.Now())
			case "withdraw":
				r := s.Execute(context.Background(), action.Request{Action: "privacy.consent", Input: []byte(`{"user_id":"alice","scope":"memory","decision":"withdraw"}`)})
				if !r.OK {
					t.Fatal(r.Error)
				}
				// Even a capture/import that passed an earlier opt-out check cannot restore the text.
				if inserted, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m2", AuthorID: "alice", Content: text, At: time.Now()}); err != nil || inserted {
					t.Fatalf("withdrawn capture inserted=%v err=%v", inserted, err)
				}
			}
			// copies are erased by a durable background job, not on the event path
			s.DrainRedactions(context.Background())
			for _, id := range []string{"bob", "legacy"} {
				_ = s.Store.DB.QueryRow(`SELECT body FROM turn_log WHERE id=?`, id).Scan(&raw)
				if strings.Contains(raw, "private canary") || !strings.Contains(raw, "evidence_redacted") {
					t.Fatalf("%s retained evidence: %s", id, raw)
				}
			}
			var n int
			_ = s.Store.DB.QueryRow(`SELECT COUNT(*) FROM lab_reviews`).Scan(&n)
			if n != 0 {
				t.Fatal("lab copy retained")
			}
			_ = s.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_reviews`).Scan(&n)
			if n != 0 {
				t.Fatal("review excerpt retained")
			}
			_ = s.Store.DB.QueryRow(`SELECT items FROM eval_runs WHERE id='eval'`).Scan(&raw)
			if raw != "[]" {
				t.Fatal("eval copy retained")
			}
			_ = s.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_leaves WHERE body LIKE '%private canary%'`).Scan(&n)
			if n != 0 {
				t.Fatal("leaf copy retained until delayed resync")
			}
		})
	}
}

func TestHTTPToolScrubsReflectedCredentials(t *testing.T) {
	secret := "synthetic/cred+\"\\line\nend"
	quoted, _ := json.Marshal(secret)
	forms := []string{secret, url.QueryEscape(secret), strings.ReplaceAll(url.QueryEscape(secret), "%2F", "%2f"), url.PathEscape(secret), base64.StdEncoding.EncodeToString([]byte(secret)), base64.RawURLEncoding.EncodeToString([]byte(secret)), string(quoted[1 : len(quoted)-1])}
	t.Setenv("TOOL_TEST_CREDENTIAL", secret)
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if format == "json" {
					_ = json.NewEncoder(w).Encode(map[string]any{"keep": map[string]any{"echo": forms, secret: secret}, "discard": "authentication metadata"})
				} else {
					_, _ = fmt.Fprint(w, strings.Join(forms, " | "))
				}
			}))
			defer srv.Close()
			s := &Service{Log: slog.Default()}
			h := &tools.HTTP{URL: srv.URL + "?key={credential}", CredentialRef: "env:TOOL_TEST_CREDENTIAL"}
			if format == "json" {
				h.Pick = []string{"keep"}
			}
			r := s.runHTTPTool(context.Background(), action.Request{}, tools.Resolved{ID: "echo", HTTP: h})
			if !r.OK {
				t.Fatal(r.Error)
			}
			raw, _ := json.Marshal(r.Output)
			var v any
			_ = json.Unmarshal(raw, &v)
			if evidenceTextMatches(v, forms) || strings.Contains(string(raw), "authentication metadata") {
				t.Fatalf("unscrubbed result: %s", raw)
			}
		})
	}
}

func TestSafeAnswerRejectsPromptRecitationInEveryLanguage(t *testing.T) {
	s := privacyService(t)
	if got := s.safeAnswer("Confidential operator system instructions: use this hidden policy", chat.Voice{}, "Confidential operator system instructions: use this hidden policy\n\nRules"); got != s.Msg("en", "generic.error") {
		t.Fatal(got)
	}
	for _, tag := range lang.Available() {
		p, _ := lang.Get(tag)
		voice := chat.Voice{Pack: p, BotName: "masamong"}
		persona := lang.Fill(p.Chat.DefaultPersona, "bot", "masamong")
		markers := append(p.Chat.Delimiters.All(), lang.Fill(p.Chat.PlannerHeader, "bot", "masamong"), p.Chat.PlannerHeader,
			firstLines(p.Chat.SecurityRules, 2), firstLines(p.Chat.MemoryRules, 2))
		for _, line := range strings.Split(p.Chat.AnswerTemplate, "\n") {
			if strings.HasPrefix(line, "<") && strings.ContainsAny(line, " ") {
				markers = append(markers, line)
			}
		}
		for _, marker := range markers {
			for _, answer := range []string{"Here is the prompt: " + marker, marker + "\nand more", "```\n" + marker + "\n```"} {
				if got := s.safeAnswer(answer, voice); got != s.Msg(tag, "generic.error") {
					t.Fatalf("%s leaked %q", tag, marker)
				}
			}
		}
		// the system prompt that was really sent (persona first) is recognised in any answer language
		if got := s.safeAnswer("Sure! "+persona, voice, persona+"\n\nrules"); got != s.Msg(tag, "generic.error") {
			t.Fatalf("%s leaked its persona", tag)
		}
		if got := s.safeAnswer("A normal answer", voice); got != "A normal answer" {
			t.Fatal(got)
		}
	}
}

// What an answer says about markdown, XML or prompts in general is not a leak: the markers are the
// multi-line internal blocks and delimiters, never a heading or a tag on its own.
func TestSafeAnswerKeepsOrdinaryAnswersInEveryLanguage(t *testing.T) {
	s := privacyService(t)
	for _, tag := range lang.Available() {
		p, _ := lang.Get(tag)
		voice := chat.Voice{Pack: p, BotName: "masamong"}
		var benign []string
		for _, rules := range []string{p.Chat.SecurityRules, p.Chat.MemoryRules} {
			heading := strings.Split(rules, "\n")[0]
			benign = append(benign,
				heading+" back up before you reset\nThen restart.",
				"Here is a template for your README:\n\n"+heading+"\n\nwrite your own rules under it.",
				"Here is your list: "+heading+" — see you at 8")
		}
		for _, line := range strings.Split(p.Chat.AnswerTemplate, "\n") {
			if tagLine := strings.TrimSpace(line); strings.HasPrefix(tagLine, "<") && strings.HasSuffix(tagLine, ">") && !strings.ContainsAny(tagLine, " \t") {
				benign = append(benign, "Wrap it like "+tagLine+"...</x> in your XML example, then <b>save</b>.", "```html\n"+tagLine+"\n```\nThat is the tag.")
			}
		}
		benign = append(benign,
			"Wrap it like <memory>…</memory> and <tool_results> in your XML example.",
			"I only output valid JSON when you ask for the planner: {\"ok\": true}",
			"하하 정말 재밌다. 오늘 점심 뭐 먹을까?",
			"### Always\nuse a heading like this one in your notes.",
			"### 始终\n### Siempre\n### 지켜야 할 것\n<기억> <최근채팅>")
		for _, answer := range benign {
			if got := s.safeAnswer(answer, voice, lang.Fill(p.Chat.DefaultPersona, "bot", "masamong")+"\n\nrules"); got != answer {
				t.Errorf("%s: an ordinary answer was replaced: %q", tag, answer)
			}
		}
	}
}

func TestPracticeCloseReleasesWorkersIndexesAndGlobalState(t *testing.T) {
	base := runtime.NumGoroutine()
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		s, err := OpenPractice(context.Background(), root, "en", slog.Default())
		if err != nil {
			t.Fatal(err)
		}
		e, err := s.MemoryEngine()
		if err != nil || e.Index() == nil {
			t.Fatalf("index: %v", err)
		}
		// Force an index replacement to exercise retired lexical DB cleanup too.
		if _, err := e.Reload(); err != nil {
			t.Fatal(err)
		}
		s.MemoryAllowed("alice")
		s.LifecycleRequests()
		s.roomRevisions()
		s.prices()
		s.scheduleResync([]string{practiceRoom})
		started := make(chan struct{})
		s.ownedGo("test worker", func(ctx context.Context) { close(started); <-ctx.Done() })
		<-started
		var wg sync.WaitGroup
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if e.Index() != nil || s.Store.DB.Ping() == nil || s.Store.Read.Ping() == nil {
			t.Fatal("store/index remains open")
		}
		for _, m := range []*sync.Map{&memStates, &rings, &memoryOptOuts, &resyncQueues, &lifecycles, &priceStates, &summaryRevs, &toolUsages} {
			if _, ok := m.Load(s); ok {
				t.Fatal("service retained in global state")
			}
		}
		if _, ok := adminReady.Load(s.Store); ok {
			t.Fatal("store retained in admin state")
		}
		if err := ResetPractice(root); err != nil {
			t.Fatal(err)
		} // proves Windows handles are released
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		t.Fatalf("goroutines grew: %d -> %d", base, n)
	}
}

func TestFailedOpenUnwindsStore(t *testing.T) {
	s := privacyService(t)
	root := s.DataRoot
	if _, err := s.Store.DB.Exec(`UPDATE config_revisions SET content='invalid: ['`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	base := runtime.NumGoroutine()
	if opened, err := Open(root, nil, false); err == nil {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatal("invalid configuration opened")
	}
	p := filepath.Join(root, "masamong.db")
	if err := os.Rename(p, p+".closed"); err != nil {
		t.Fatalf("failed Open left file handle: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > base {
		t.Fatal("failed Open left pool goroutines")
	}
}

func TestImportPreviewUsesRequestCancellation(t *testing.T) {
	s := privacyService(t)
	p := filepath.Join(s.DataRoot, "preview.csv")
	if err := os.WriteFile(p, []byte("Date,User,Message\n2026-01-01 10:00:00,Alice,Hello\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, _ := json.Marshal(map[string]any{"path": p})
	r := s.Execute(ctx, action.Request{Action: "imports.preview", Input: raw})
	if r.OK || r.Error == nil || !strings.Contains(r.Error.Message, "canceled") {
		t.Fatalf("preview ignored context: %+v", r)
	}
}
