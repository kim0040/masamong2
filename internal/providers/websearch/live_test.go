package websearch

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"masamong/internal/providers"
)

// liveKinds maps the key file names of MASAMONG_LIVE_KEYS_DIR (the provider ids the product uses) to
// the search kinds, in the order the operator runs them.
var liveKinds = []struct{ id, kind string }{
	{"exa", "exa"}, {"tinyfish", "tinyfish"}, {"linkup", "linkup"}, {"kakao", "kakao_search"},
	{"tavily", "tavily"}, {"brave", "brave"}, {"serper", "serper"},
}

// liveServices builds the services that have a key file, in MASAMONG_LIVE_SEARCH order (default: the
// operator's order exa, tinyfish, linkup, kakao). The key values are never printed.
func liveServices(t *testing.T) (map[string]Searcher, []string) {
	t.Helper()
	dir := os.Getenv("MASAMONG_LIVE_KEYS_DIR")
	if dir == "" || os.Getenv("CI") != "" {
		t.Skip("set MASAMONG_LIVE_KEYS_DIR to a folder of key files (never in CI)")
	}
	t.Setenv("MASAMONG_OFFLINE", "")
	want := strings.Split(os.Getenv("MASAMONG_LIVE_SEARCH"), ",")
	if os.Getenv("MASAMONG_LIVE_SEARCH") == "" {
		want = []string{"exa", "tinyfish", "linkup", "kakao"}
	}
	out := map[string]Searcher{}
	var order []string
	for _, id := range want {
		id = strings.TrimSpace(id)
		kind := ""
		for _, k := range liveKinds {
			if k.id == id {
				kind = k.kind
			}
		}
		if kind == "" {
			t.Fatalf("MASAMONG_LIVE_SEARCH: unknown service %q", id)
		}
		path := filepath.Join(dir, id)
		if _, err := os.Stat(path); err != nil {
			t.Logf("%s: no key file, skipped", id)
			continue
		}
		cred, err := providers.Resolve("file:" + path)
		if err != nil || !cred.Present() {
			t.Logf("%s: key file unusable, skipped", id)
			continue
		}
		c := providers.NewClient(90*time.Second, nil)
		c.MaxRetries = 0 // the raw latency of one request
		s, err := New(kind, c, "", cred)
		if err != nil {
			t.Fatal(err)
		}
		out[id], order = s, append(order, id)
	}
	if len(order) == 0 {
		t.Skip("no key file for MASAMONG_LIVE_SEARCH")
	}
	return out, order
}

type liveRow struct {
	service, combo string
	ms             int64
	results, body  int
	dated          int
	avgRunes       int
	cost           string
	err            string
}

func measure(s Searcher, q Query) (liveRow, Response) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := s.Search(ctx, q)
	row := liveRow{ms: time.Since(start).Milliseconds(), results: len(res.Results), cost: "-"}
	total := 0
	for _, r := range res.Results {
		if n := utf8.RuneCountInString(r.Snippet); n > 0 {
			row.body++
			total += n
		}
		if r.Published != "" {
			row.dated++
		}
	}
	if row.body > 0 {
		row.avgRunes = total / row.body
	}
	if res.CostUSD != nil {
		row.cost = strconv.FormatFloat(*res.CostUSD, 'f', 4, 64)
	}
	if err != nil {
		row.err = strings.ReplaceAll(err.Error(), "\n", " ")
		if len(row.err) > 160 {
			row.err = row.err[:160]
		}
	}
	return row, res
}

func (r liveRow) markdown() string {
	e := r.err
	if e == "" {
		e = "-"
	}
	return fmt.Sprintf("| %s | %s | %d | %d | %d | %d/%d | %d | %s | %s |", r.service, r.combo, r.ms, r.results, r.body, r.dated, r.results, r.avgRunes, r.cost, e)
}

// TestLiveSearchServices checks each search service with a key against its real API: the request
// shape is accepted, the answer parses, and results carry a title, an address and text. It spends cents
// (Exa is paid), so it runs only when MASAMONG_LIVE_KEYS_DIR names a folder of key files named by
// provider id (exa, tinyfish, linkup, kakao; also tavily, brave, serper) and never in CI:
//
//	MASAMONG_LIVE_KEYS_DIR=~/keys go test ./internal/providers/websearch -run TestLiveSearchServices -v
//
// MASAMONG_LIVE_SEARCH picks the services and their order (default exa,tinyfish,linkup,kakao).
// MASAMONG_LIVE_SEARCH_MATRIX=1 runs the depth × topic × freshness × language table instead of the
// one regression call per service, and MASAMONG_LIVE_SEARCH_REPORT names a Markdown file for it;
// MASAMONG_LIVE_EXA_BUDGET (dollars, default 0.30) stops Exa calls once its reported cost adds up to it.
func TestLiveSearchServices(t *testing.T) {
	services, order := liveServices(t)
	budget := 0.30
	if v, err := strconv.ParseFloat(os.Getenv("MASAMONG_LIVE_EXA_BUDGET"), 64); err == nil && v > 0 {
		budget = v
	}
	spent := 0.0
	var report []string
	run := func(id, combo string, q Query) liveRow {
		t.Helper()
		if id == "exa" && spent >= budget {
			return liveRow{service: id, combo: combo, err: "skipped: Exa budget used", cost: "-"}
		}
		row, res := measure(services[id], q)
		row.service, row.combo = id, combo
		if res.CostUSD != nil {
			spent += *res.CostUSD
		}
		t.Logf("%s", row.markdown())
		report = append(report, row.markdown())
		return row
	}
	if os.Getenv("MASAMONG_LIVE_SEARCH_MATRIX") != "1" {
		for _, id := range order {
			row := run(id, "standard ko", Query{Text: "서울 날씨 예보", Max: 3})
			if row.err != "" || row.results == 0 || row.body == 0 {
				t.Errorf("%s: %+v", id, row)
			}
			if NewsSearch(services[id].Kind()) {
				if row := run(id, "news week ko", Query{Text: "코스피 시황", Max: 3, Topic: TopicNews, Fresh: "week"}); row.err != "" || row.results == 0 || row.dated == 0 {
					t.Errorf("%s news: %+v", id, row)
				}
			}
			// papers: every service answers a research search its own way (its search of papers, preferred
			// scholarly sites or the word for papers); the titles are logged to judge them by eye
			for _, rq := range []struct{ name, text string }{{"research ko", "시간이 3차원이라는 물리학"}, {"research en", "three dimensional time physics"}} {
				row, res := measure(services[id], Query{Text: rq.text, Max: 5, Topic: TopicResearch})
				row.service, row.combo = id, rq.name
				t.Logf("%s", row.markdown())
				for _, r := range res.Results {
					host := r.URL
					if u, err := url.Parse(r.URL); err == nil {
						host = u.Host
					}
					t.Logf("    %s | %s | %s", host, r.Published, r.Title)
				}
				if row.err != "" || row.results == 0 {
					t.Errorf("%s %s: %+v", id, rq.name, row)
				}
			}
		}
		return
	}
	type combo struct {
		name string
		q    Query
	}
	ko, en := "오늘 비트코인 왜 올랐대", "Seoul subway fare increase"
	combos := []combo{
		{"standard ko", Query{Text: ko, Max: 5}},
		{"quick ko", Query{Text: ko, Max: 5, Depth: DepthQuick}},
		{"deep ko", Query{Text: ko, Max: 8, Depth: DepthDeep}},
		{"standard en", Query{Text: en, Max: 5}},
		{"quick en", Query{Text: en, Max: 5, Depth: DepthQuick}},
		{"news standard ko day", Query{Text: ko, Max: 5, Topic: TopicNews, Fresh: "day"}},
		{"news standard en week", Query{Text: en, Max: 5, Topic: TopicNews, Fresh: "week"}},
		{"news deep ko", Query{Text: "이번 주 AI 반도체 뉴스", Max: 8, Topic: TopicNews, Depth: DepthDeep}},
		{"fresh day ko", Query{Text: ko, Max: 5, Fresh: "day"}},
		{"fresh week en", Query{Text: en, Max: 5, Fresh: "week"}},
		{"size 1", Query{Text: ko, Max: 1}},
		{"size 10", Query{Text: ko, Max: 10}},
	}
	for _, id := range order {
		for _, c := range combos {
			run(id, c.name, c.q)
		}
	}
	if spent > 0 {
		report = append(report, fmt.Sprintf("\nExa cost reported: $%.4f of $%.2f", spent, budget))
		t.Logf("Exa cost reported: $%.4f of $%.2f", spent, budget)
	}
	if path := os.Getenv("MASAMONG_LIVE_SEARCH_REPORT"); path != "" {
		head := "| service | combo | ms | results | with text | dated | avg text runes | cost $ | error |\n|---|---|---|---|---|---|---|---|---|\n"
		_ = os.WriteFile(path, []byte(head+strings.Join(report, "\n")+"\n"), 0o600)
	}
}
