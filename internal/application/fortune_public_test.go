package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/tools"
)

func asIn(svc *application.Service, loc, act string, in map[string]any, inv *action.Invoker) action.Result {
	raw, _ := json.Marshal(in)
	return svc.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: loc, Invoker: inv})
}

// promptRecorder is a model that records the user prompt of each call and answers with text.
func promptRecorder(t *testing.T, text string) (url string, last func() string) {
	t.Helper()
	var mu sync.Mutex
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		prompts = append(prompts, body.Messages[len(body.Messages)-1].Content)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(prompts) == 0 {
			return ""
		}
		return prompts[len(prompts)-1]
	}
}

// A public channel's summary is written from the sun sign and the day only: the person's birth date,
// birth time, birthplace and natal pillars are not in the prompt (the model's text is posted as written).
// 1995-06-15 23:40 in Seoul is 을해년 임오월 정축일 (lunar-go at 22:40 Beijing time, 야자시); the private
// prompt has them, the public one in Korean and English does not. The registered gender is private too:
// the public template has no {gender}.
func TestFortunePublicPromptHasNoBirthData(t *testing.T) {
	url, last := promptRecorder(t, "SUMMARY: 좋아요\nWORK: 집중")
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled = true
		d.I18n.Timezone = "Asia/Seoul"
		if d.Tools == nil {
			d.Tools = map[string]tools.Settings{}
		}
		d.Tools["fortune.query"] = tools.Settings{Defaults: map[string]any{"daily_limit": 10}}
	})
	serveAnyServer(t, svc)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 15, 8, 0, 0, 0, seoul)}
	dm := person("u1", true)
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
	if r := as(svc, "fortune.subscribe", map[string]any{"op": "register", "text": "1995-06-15 23:40 남 부산광역시"}, dm); r.Output["status"] != "registered" {
		t.Fatalf("register: %+v", r)
	}
	natal := map[string][]string{"ko": {"을해", "임오", "정축", "타고난 사주"}, "en": {"Yi-Hai", "Ren-Wu", "Ding-Chou", "Natal pillars"}}
	personal := []string{"1995-06-15", "23:40", "부산광역시"}
	gender := map[string]string{"ko": "남성", "en": "male"}
	for _, loc := range []string{"ko", "en"} {
		if r := asIn(svc, loc, "fortune.query", map[string]any{"what": "today"}, dm); r.Output["status"] != "ok" {
			t.Fatalf("%s private: %+v", loc, r)
		}
		wantAll(t, loc+" private prompt", last(), append(append(append([]string{}, personal...), natal[loc]...), gender[loc])...)
		if r := asIn(svc, loc, "fortune.query", map[string]any{"what": "today"}, person("u1", false)); r.Output["status"] != "ok" {
			t.Fatalf("%s public: %+v", loc, r)
		}
		p := last()
		wantNone(t, loc+" public prompt", p, append(append(append([]string{}, personal...), natal[loc]...), gender[loc])...)
		wantAll(t, loc+" public prompt", p, map[string]string{"ko": "쌍둥이자리", "en": "Gemini"}[loc], "2026-10-15")
	}
}
