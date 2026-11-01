package application_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

// postingGateway is a connected Discord that records the bot's own posts and edits.
type postingGateway struct {
	fakeGateway
	mu    sync.Mutex
	posts []string // channel|title|text
	edits []string // channel|message|title|text
	n     int
}

func (g *postingGateway) PostChannel(_ context.Context, channelID, _, _ string, card *action.Card) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	g.posts = append(g.posts, channelID+"|"+card.Title+"|"+card.Text)
	return fmt.Sprint("m", g.n), nil
}

func (g *postingGateway) EditChannel(_ context.Context, channelID, messageID string, card *action.Card) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.edits = append(g.edits, channelID+"|"+messageID+"|"+card.Title+"|"+card.Text)
	return nil
}

// kmaFeed serves getEqkMsg and wrn_now_data from what the test sets (shapes recorded 2026-10-01).
type kmaFeed struct {
	mu       sync.Mutex
	quakes   []string
	warnings []string
}

func (f *kmaFeed) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.Contains(r.URL.Path, "getEqkMsg") {
			if len(f.quakes) == 0 {
				_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"03","resultMsg":"NO_DATA"}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"00","resultMsg":"NORMAL_SERVICE"},"body":{"items":{"item":[` + strings.Join(f.quakes, ",") + `]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`[` + strings.Join(f.warnings, ",") + `]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func quake(tm, loc string, lat, lon, mt float64, rem string) string {
	return fmt.Sprintf(`{"tmFc":%s,"tmEqk":%s00,"loc":%q,"lat":%v,"lon":%v,"mt":%v,"dep":12,"rem":%q,"fcTp":3}`, tm, tm, loc, lat, lon, mt, rem)
}

func warning(regID, parent, region, kind, level, cmd string) string {
	return fmt.Sprintf(`{"REG_UP":"L1000000","REG_UP_KO":%q,"REG_ID":%q,"REG_KO":%q,"TM_FC":"202610011100","TM_EF":"202610011200","WRN":%q,"LVL":%q,"CMD":%q,"ED_TM":""}`, parent, regID, region, kind, level, cmd)
}

// The bot watches KMA on its own: the first check only sets a starting point, a new earthquake is
// posted to every answering channel (not one that turned announcements off, not an overseas quake
// below 7.0, never a 국내영향없음 notice), an aftershock edits the incident's message, nothing repeats,
// and weather warnings are announced as issued and lifted (경보 anywhere, 주의보 only in chosen regions).
func TestDisasterAlertsWatchKMAAndAnnounce(t *testing.T) {
	feed := &kmaFeed{}
	srv := feed.server(t)
	svc := setupSvc(t)
	pointAt(t, svc, "kma", srv.URL, "KMA_TEST_KEY", "k")
	for _, ch := range []map[string]any{{"id": "c1", "respond": "mention"}, {"id": "c2", "respond": "always"}, {"id": "c3", "respond": "off"}} {
		ch["guild_id"] = "g1"
		if r := exec(svc, "channels.configure", ch); !r.OK {
			t.Fatal(r.Error)
		}
	}
	applyDoc(t, svc, func(d *config.Document) {
		g := d.Guilds["g1"]
		g.Locale = "ko" // announcements speak the server's language
		c := g.Channels["c2"]
		c.DisasterAlerts = "off"
		g.Channels["c2"] = c
		d.Guilds["g1"] = g
		d.DisasterAlerts.WarningRegions = []string{"서울"}
	})
	gw := &postingGateway{}
	svc.Discord = gw
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	tick := func(d time.Duration) {
		now = now.Add(d)
		svc.Clock = clock.Fixed{T: now}
		application.DisasterTickForTest(svc, ctx)
	}
	// an old quake already published when watching starts is not news
	feed.quakes = []string{quake("202609300900", "경북 경주시 남남서쪽 9km 지역", 35.76, 129.19, 4.2, "")}
	tick(time.Minute)
	if len(gw.posts) != 0 {
		t.Fatalf("the first check only sets the starting point: %v", gw.posts)
	}
	// a new domestic quake, an overseas one below 7.0 and a no-impact notice
	feed.quakes = append(feed.quakes,
		quake("202610011000", "경북 경주시 남서쪽 10km 지역", 35.78, 129.17, 4.5, "지진으로 인해 피해가 없도록 안전에 유의하시기 바랍니다."),
		quake("202610011005", "일본 오키나와현 남쪽 해역", 24.0, 125.0, 6.4, ""),
		quake("202610011010", "일본 도쿄도 해역", 35.0, 140.0, 7.5, "국내영향없음."))
	tick(time.Minute)
	// the quake already published the day before is part of the same incident (72 h, 150 km)
	if len(gw.posts) != 1 || !strings.HasPrefix(gw.posts[0], "c1|🌏 국내 지진 발생|**규모 4.5") || !strings.Contains(gw.posts[0], "같은 지역 지진 2건") {
		t.Fatalf("one post, the answering channel that did not turn it off: %v", gw.posts)
	}
	var savedAt string
	if err := svc.Store.DB.QueryRow(`SELECT updated_at FROM admin_state WHERE k='disaster:state'`).Scan(&savedAt); err != nil {
		t.Fatal(err)
	}
	tick(time.Minute)
	var unchangedAt string
	if err := svc.Store.DB.QueryRow(`SELECT updated_at FROM admin_state WHERE k='disaster:state'`).Scan(&unchangedAt); err != nil {
		t.Fatal(err)
	}
	if unchangedAt != savedAt {
		t.Fatalf("a successful check with no new state must not write disaster:state: %s -> %s", savedAt, unchangedAt)
	}
	if len(gw.posts) != 1 || len(gw.edits) != 0 {
		t.Fatalf("nothing new, nothing sent: posts %v edits %v", gw.posts, gw.edits)
	}
	// an aftershock edits the incident's message
	feed.quakes = append(feed.quakes, quake("202610011030", "경북 경주시 남서쪽 11km 지역", 35.77, 129.16, 3.1, ""))
	tick(time.Minute)
	tick(time.Minute)
	if len(gw.posts) != 1 || len(gw.edits) != 0 {
		t.Fatalf("a 3.1 aftershock is below the floor: %v %v", gw.posts, gw.edits)
	}
	feed.quakes = append(feed.quakes, quake("202610011100", "경북 경주시 남서쪽 12km 지역", 35.77, 129.15, 4.1, ""))
	tick(time.Minute)
	if len(gw.edits) != 1 || !strings.HasPrefix(gw.edits[0], "c1|m1|") || !strings.Contains(gw.edits[0], "같은 지역 지진 3건") {
		t.Fatalf("the aftershock edits the incident's message: %v", gw.edits)
	}
	// weather warnings: the first check (with the first earthquake check) found none in force
	feed.warnings = []string{
		warning("L1100100", "서울·인천·경기도", "서울동남권", "폭염", "주의", "발표"),
		warning("L1200100", "부산·울산·경상남도", "부산", "호우", "경보", "발표"),
		warning("L1300100", "대구·경상북도", "대구", "건조", "주의", "발표"),
		`{"REG_UP":"S1130000","REG_UP_KO":"동해남부전해상","REG_ID":"S1132210","REG_KO":"동해남부북쪽안쪽먼바다","TM_FC":"202610010700","TM_EF":"202610010800","WRN":"풍랑","LVL":"경보","CMD":"발표","ED_TM":""}`,
	}
	before := len(gw.posts)
	tick(5 * time.Minute)
	got := strings.Join(gw.posts[before:], "\n")
	if len(gw.posts)-before != 2 || !strings.Contains(got, "c1|⚠️ 폭염주의보 발표|서울동남권") || !strings.Contains(got, "c1|⚠️ 호우경보 발표|부산") {
		t.Fatalf("경보 anywhere on land, 주의보 only in the chosen region, no sea area: %s", got)
	}
	tick(time.Minute)
	if len(gw.posts)-before != 2 {
		t.Fatalf("warnings are checked every five minutes: %v", gw.posts[before:])
	}
	feed.warnings = feed.warnings[1:]
	tick(5 * time.Minute)
	if last := gw.posts[len(gw.posts)-1]; len(gw.posts)-before != 3 || !strings.HasPrefix(last, "c1|✅ 폭염주의보 해제|서울동남권") {
		t.Fatalf("a lifted warning is announced: %v", gw.posts[before:])
	}
	st := out(t, svc, "disasters.alerts", nil)
	if st["channels"].(float64) != 1 || st["waiting"] != "" || st["state"].(map[string]any)["last_sent_kind"] != "warning" {
		t.Fatalf("status: %v", st)
	}
	// switched off: nothing is checked or sent
	applyDoc(t, svc, func(d *config.Document) { off := false; d.DisasterAlerts.Enabled = &off })
	feed.warnings = append(feed.warnings, warning("L1400100", "광주·전라남도", "광주", "태풍", "경보", "발표"))
	n := len(gw.posts)
	tick(10 * time.Minute)
	if len(gw.posts) != n || out(t, svc, "disasters.alerts", nil)["waiting"] != "off" {
		t.Fatalf("off means off: %v", gw.posts[n:])
	}
}

// The watcher writes only what changed: with warnings on and the same ones still in force, a check
// leaves every stored key alone, yet the console's "last checked" keeps moving (it is kept in memory).
func TestDisasterAlertsIdleChecksWriteNothingButStatusStaysFresh(t *testing.T) {
	feed := &kmaFeed{warnings: []string{warning("L1200100", "부산·울산·경상남도", "부산", "호우", "경보", "발표")}}
	srv := feed.server(t)
	svc := setupSvc(t)
	pointAt(t, svc, "kma", srv.URL, "KMA_TEST_KEY", "k")
	if r := exec(svc, "channels.configure", map[string]any{"id": "c1", "guild_id": "g1", "respond": "mention"}); !r.OK {
		t.Fatal(r.Error)
	}
	svc.Discord = &postingGateway{}
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	tick := func(d time.Duration) {
		now = now.Add(d)
		svc.Clock = clock.Fixed{T: now}
		application.DisasterTickForTest(svc, ctx)
	}
	stamps := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		rows, err := svc.Store.DB.Query(`SELECT k, updated_at FROM admin_state WHERE k LIKE 'disaster:%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k, at string
			if err := rows.Scan(&k, &at); err != nil {
				t.Fatal(err)
			}
			out[k] = at
		}
		return out
	}
	tick(time.Minute) // the starting point: what is in force now is recorded
	before := stamps()
	if before["disaster:wrn_active"] == "" || before["disaster:eqk_mark"] == "" {
		t.Fatalf("the first check should store its starting point: %v", before)
	}
	for i := 0; i < 4; i++ {
		tick(5 * time.Minute) // each one re-checks warnings (five minutes passed) and finds the same set
	}
	if after := stamps(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("idle checks wrote to the database:\nbefore %v\nafter  %v", before, after)
	}
	state := out(t, svc, "disasters.alerts", nil)["state"].(map[string]any)
	want := now.UTC().Format(time.RFC3339)
	if state["checked_at"] != want || state["warnings_checked_at"] != want {
		t.Fatalf("console last-checked is stale: %v, want %s", state, want)
	}
}
