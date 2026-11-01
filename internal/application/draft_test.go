package application_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"masamong/internal/config"
)

// A lab turn with draft rules and style uses them; the saved channel settings are untouched.
func TestLabTurnTriesDraftRules(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		prompts = append(prompts, string(b))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{"memory_requirement":"none","tool_plan":[]}`}, "finish_reason": "stop"}}})
	}))
	defer srv.Close()
	svc := routedSvc(t, srv.URL, srv.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Rules: "저장된 규칙"}}}}
	})
	r := exec(svc, "chat.respond", map[string]any{"guild_id": "g", "channel_id": "c", "message": "안녕", "draft_rules": "초안 규칙: 존댓말", "draft_style": "초안 말투: 해적처럼"})
	if !r.OK {
		t.Fatalf("%+v", r)
	}
	all := strings.Join(prompts, "\n")
	if !strings.Contains(all, "초안 규칙") || !strings.Contains(all, "해적처럼") || strings.Contains(all, "저장된 규칙") {
		t.Fatal("the draft replaces the saved rules and style in a lab turn")
	}
	if svc.ChannelPolicyFor("g", "c").Rules != "저장된 규칙" {
		t.Fatal("a draft is never saved")
	}
}
