package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// fakeUniversity serves a school board, its notice pages and a transfer admission board.
func fakeUniversity(t *testing.T, hits *atomic.Int32) (string, string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case r.URL.Path == "/robots.txt":
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
		case r.URL.Path == "/school/list":
			fmt.Fprint(w, `<table><tbody>
<tr><td class="t"><a href="/school/view/101">국가장학금 2차 신청 안내</a></td><td class="d">2026.09.25</td></tr>
<tr><td class="t"><a href="/school/view/102">대학원 논문 심사 일정</a></td><td class="d">2026.09.24</td></tr>
<tr><td class="t"><a href="/school/view/103">생활관 소방 점검 안내</a></td><td class="d">2026.09.23</td></tr>
</tbody></table>`)
		case r.URL.Path == "/school/view/101":
			fmt.Fprint(w, `<div class="body">학부 재학생 대상 국가장학금 2차 신청을 받습니다. 신청 기간: 2026.10.02 ~ 2026.10.10</div>`)
		case r.URL.Path == "/school/view/102":
			fmt.Fprint(w, `<div class="body">대학원생 석사과정 논문 심사 일정을 알립니다.</div>`)
		case r.URL.Path == "/school/view/103":
			fmt.Fprint(w, `<div class="body">생활관 소방 점검이 있습니다.</div>`)
		case r.URL.Path == "/transfer/list":
			fmt.Fprint(w, `<ul><li><a href="/transfer/view?id=1">2027학년도 편입학 모집요강 안내</a> 2026-09-20</li>
<li><a href="/transfer/view?id=2">2027학년도 대학원 신입생 모집</a></li></ul>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return srv.URL, u.Hostname()
}

func noticeCatalog(base, host string) string {
	cat := map[string]any{
		"schema_version": 1,
		"schools":        []any{map[string]any{"id": "test", "name": "테스트대학교"}},
		"vocabulary": map[string]any{
			"audiences": []any{
				map[string]any{"id": "undergraduate", "label": "학부생", "terms": []string{"학부생", "학부 재학생"}},
				map[string]any{"id": "graduate", "label": "대학원생", "terms": []string{"대학원생", "석사과정"}},
			},
			"topics": []any{
				map[string]any{"id": "scholarship", "label": "장학", "terms": []string{"장학"}},
				map[string]any{"id": "dormitory", "label": "기숙사", "terms": []string{"생활관"}},
			},
			"actions":            []any{map[string]any{"id": "apply", "label": "신청", "terms": []string{"신청"}}},
			"degree_audiences":   map[string]any{"undergraduate": []string{"undergraduate"}, "master": []string{"graduate"}},
			"opportunity_topics": []string{"scholarship"},
		},
		"transfer_filters": map[string]any{"announcement": "(?:편입|모집)", "exclude": "(?:대학원)"},
		"detail_fallback":  map[string]any{"containers": []string{"body"}},
		"sources": []any{
			map[string]any{"id": "test_school", "kind": "school", "school": "test", "university": "테스트대학교", "board": "공지",
				"list_url": base + "/school/list", "allowed_hosts": []string{host},
				"list":   map[string]any{"row_selector": "tbody tr", "link_selector": "td.t a", "id_attribute": "href", "id_regex": `/view/(\d+)`, "date_selector": "td.d"},
				"detail": map[string]any{"body_selectors": []string{".body"}}},
			map[string]any{"id": "transfer_test", "kind": "transfer", "university": "테스트대학교", "list_url": base + "/transfer/list",
				"allowed_hosts": []string{host}, "list": map[string]any{"link_selector": "a[href]", "href_regex": `/transfer/view`}},
			map[string]any{"id": "transfer_manual", "kind": "transfer", "university": "수동대학교", "official_url": "https://manual.example/", "manual": true},
		},
	}
	b, _ := json.Marshal(cat)
	return string(b)
}

// collect runs a collection, waiting for one already running to finish first.
func collect(t *testing.T, svc *application.Service, only []string) application.CollectReport {
	t.Helper()
	for i := 0; i < 100; i++ {
		rep, err := application.CollectNotices(svc, context.Background(), only)
		if err == nil {
			return rep
		}
		if !strings.Contains(err.Error(), "already running") {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("collection never finished")
	return application.CollectReport{}
}

func TestNoticesCollectPersonalizeAndDeliver(t *testing.T) {
	var hits atomic.Int32
	base, host := fakeUniversity(t, &hits)
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	application.AllowLocalNoticeBoards(svc)
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools = map[string]tools.Settings{"notices.status": {Defaults: map[string]any{"host_interval_ms": 1}}}
	})
	dir := filepath.Join(svc.DataRoot, "notices")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(noticeCatalog(base, host)), 0o600); err != nil {
		t.Fatal(err)
	}
	u := person("u1", true)
	// consent first
	r := as(svc, "notices.subscribe", map[string]any{"kind": "school", "op": "register", "text": "테스트대 학부 2학년"}, u)
	if r.Output["status"] != "consent_required" {
		t.Fatalf("consent: %+v", r)
	}
	_ = as(svc, "privacy.consent", map[string]any{"scope": "school_notice", "decision": "grant", "confirmed": true}, u)
	if r := as(svc, "notices.subscribe", map[string]any{"kind": "school", "op": "register", "text": "없는대 학부"}, u); r.OK {
		t.Fatal("unknown school")
	}
	r = as(svc, "notices.subscribe", map[string]any{"kind": "school", "op": "register", "text": "테스트대 학부 2학년", "time": "8:00"}, u)
	if !r.OK || r.Output["status"] != "saved" {
		t.Fatalf("register: %+v", r)
	}
	card := r.Output["card"].(action.Card)
	if card.Fields[0].Value != "테스트대학교" || card.Fields[2].Value != "2" {
		t.Fatalf("parsed profile: %+v", card.Fields)
	}
	// nothing is read for boards nobody needs; the school board is read with its notice pages
	collect(t, svc, nil) // the save started a collection of its own; this one waits for it
	status := exec(svc, "notices.status", nil)
	boards := status.Output["boards"].([]map[string]any)
	var school map[string]any
	for _, b := range boards {
		if b["id"] == "test_school" {
			school = b
		}
		if b["id"] == "transfer_test" && b["state"] != nil {
			t.Fatal("a board nobody subscribed to must not be read")
		}
	}
	if school == nil || school["state"] == nil || school["state"].(map[string]any)["items"] != 3 {
		t.Fatalf("school board: %+v", school)
	}
	// picks: the undergraduate scholarship notice, not the graduate one
	latest := as(svc, "notices.status", map[string]any{"kind": "school"}, u)
	text := latest.Output["card"].(action.Card).Text
	if !strings.Contains(text, "국가장학금") || strings.Contains(text, "논문") {
		t.Fatalf("picks: %s", text)
	}
	// mute a topic
	if r := as(svc, "notices.subscribe", map[string]any{"kind": "school", "op": "mute", "topic": "장학"}, u); !r.OK {
		t.Fatalf("mute: %+v", r)
	}
	if text := as(svc, "notices.status", map[string]any{"kind": "school"}, u).Output["card"].(action.Card).Text; strings.Contains(text, "국가장학금") {
		t.Fatalf("muted topic still shown: %s", text)
	}
	_ = as(svc, "notices.subscribe", map[string]any{"kind": "school", "op": "unmute", "topic": "장학"}, u)
	// delivery at 08:00 in the person's zone, once
	gw := &dmGateway{}
	svc.Discord = gw
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc.Clock = clock.Fixed{T: time.Now().In(seoul).Truncate(24 * time.Hour).Add(30 * time.Hour)} // tomorrow, well after 08:00
	application.DeliverNotices(svc, context.Background())
	application.DeliverNotices(svc, context.Background())
	if len(gw.sent) != 1 || !strings.Contains(gw.sent[0].Text, "국가장학금") {
		t.Fatalf("delivered: %+v", gw.sent)
	}
	// transfer: public recent list needs no consent; subscribing does. The clock goes back to real time
	// before subscribing starts a background collection that reads it.
	svc.Clock = clock.Real{}
	_ = as(svc, "privacy.consent", map[string]any{"scope": "transfer_notice", "decision": "grant", "confirmed": true}, u)
	if r := as(svc, "notices.subscribe", map[string]any{"kind": "transfer", "op": "subscribe", "universities": "화성대"}, u); r.OK {
		t.Fatal("unknown university")
	}
	if r := as(svc, "notices.subscribe", map[string]any{"kind": "transfer", "op": "subscribe", "universities": "테스트대"}, u); !r.OK {
		t.Fatalf("transfer subscribe: %+v", r)
	}
	collect(t, svc, []string{"transfer_test"})
	recent := as(svc, "transfer.recent", nil, person("u9", false))
	items := recent.Output["items"].([]map[string]any)
	if len(items) != 1 || !strings.Contains(items[0]["title"].(string), "편입학") {
		t.Fatalf("transfer recent: %+v", recent.Output)
	}
	// deleting withdraws consent and removes the settings
	if r := as(svc, "notices.subscribe", map[string]any{"kind": "school", "op": "delete"}, u); !r.OK || svc.ConsentAllows("u1", "school_notice") {
		t.Fatalf("delete: %+v", r)
	}
	if hits.Load() == 0 {
		t.Fatal("the fake site was never read")
	}
}
