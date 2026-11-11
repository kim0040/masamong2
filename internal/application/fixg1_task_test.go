package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

// taskWithSearch schedules a daily task whose run plans plan (a web search) and runs it once; it returns
// the stored record of the run.
func taskWithSearch(t *testing.T, plan string, services func(svc *application.Service)) map[string]any {
	t.Helper()
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc, _ := chatSvc(t, plan)
	serveAnyServer(t, svc)
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled = true
		d.I18n.Timezone = "Asia/Seoul"
		d.Tools = nil // chatSvc keeps search.web out of chat; a task may search
	})
	services(svc)
	gw := &postGateway{}
	svc.Discord = gw
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 15, 20, 0, 0, 0, seoul)}
	if r := as(svc, "reminders.propose", map[string]any{"kind": "task", "when": "오전 9시", "repeat": "daily", "what": "캠핑장 요금",
		"task": "가평 캠핑장 요금을 찾아 정리해 줘"}, viaChat("u1")); r.Output["status"] != "scheduled" {
		t.Fatalf("set: %+v", r)
	}
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 16, 9, 0, 20, 0, seoul)}
	application.DeliverReminders(svc, context.Background())
	if gw.postCount() != 1 {
		t.Fatalf("posted: %+v", gw.posts)
	}
	for _, it := range out(t, svc, "turns.list", nil)["turns"].([]any) {
		turn := out(t, svc, "turns.get", map[string]any{"id": it.(map[string]any)["id"]})["turn"].(map[string]any)
		if body, _ := turn["body"].(map[string]any); body["scheduled"] != nil {
			return body
		}
	}
	t.Fatal("no record of the task's run")
	return nil
}

// recordedSearch is the first search.web result of a turn record's tools.
func recordedSearch(t *testing.T, body map[string]any) []any {
	t.Helper()
	tools, _ := body["tools"].([]any)
	for _, x := range tools {
		if tr := x.(map[string]any); tr["tool"] == "search.web" {
			data, _ := tr["data"].(map[string]any)
			list, _ := data["results"].([]any)
			if len(list) == 0 {
				t.Fatalf("search without results: %v", tr)
			}
			return list
		}
	}
	t.Fatalf("no search in the record: %v", body["tools"])
	return nil
}

// A scheduled task's web search opens no page (it is not a member waiting in the chat), and its record
// keeps at most 200 runes of any passage a search did bring, as a chat turn's record does.
func TestScheduledTaskReadsNoPagesAndClipsItsRecord(t *testing.T) {
	plan := `{"memory_requirement":"none","question_kind":"other","tool_plan":[{"tool_name":"search.web","parameters":{"query":"가평 캠핑장 요금"}}]}`
	var fake *fetchFake
	body := taskWithSearch(t, plan, func(svc *application.Service) {
		pointAt(t, svc, "serper", serperServer(t, 5, hostOf).URL, "S_KEY", "k")
		pointAt(t, svc, "tinyfish", "http://127.0.0.1:1", "TF_KEY", "tf")
		fake = newFetchFake(t, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })
	})
	if fake.count() != 0 {
		t.Fatalf("the task's search opened %d pages", fake.count())
	}
	for _, it := range recordedSearch(t, body) {
		if item := it.(map[string]any); item["passage"] != nil || item["read"] != nil {
			t.Fatalf("a page was read: %v", item)
		}
	}

	// a planner that asks for pages gets them from the service's own text (Exa opens no page): the record
	// keeps 200 runes of each
	text := strings.Repeat("가평 캠핑장은 강 옆에 있고 1박에 4만 원입니다. ", 60)
	exa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"results": []any{map[string]any{"title": "E1", "url": "https://c.example/1", "highlights": []string{"h"}, "text": text}},
			"costDollars": map[string]any{"total": 0.007}})
		_, _ = w.Write(b)
	}))
	defer exa.Close()
	plan = `{"memory_requirement":"none","question_kind":"other","tool_plan":[{"tool_name":"search.web","parameters":{"query":"가평 캠핑장 요금","pages":2}}]}`
	body = taskWithSearch(t, plan, func(svc *application.Service) { pointAt(t, svc, "exa", exa.URL, "E_KEY", "k") })
	item := recordedSearch(t, body)[0].(map[string]any)
	passage, _ := item["passage"].(string)
	if passage == "" || utf8.RuneCountInString(passage) > 201 || item["passage_runes"] == nil {
		t.Fatalf("recorded passage of %d runes: %v", utf8.RuneCountInString(passage), item)
	}
}
