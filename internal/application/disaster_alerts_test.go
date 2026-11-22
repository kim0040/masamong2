package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	text     []string // typ01 rows (REG_UP,…,CMD) served instead of the JSON warnings when set
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
		if f.text != nil {
			_, _ = w.Write([]byte("#START7777\n#  REG_UP, REG_UP_KO, REG_ID, REG_KO, TM_FC, TM_EF, WRN, LVL, CMD, ED_TM, --\n" + strings.Join(f.text, "\n") + "\n#7777END\n"))
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
// and weather warnings are announced as issued and lifted according to each channel’s watch levels.
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
		room := g.Channels["c1"]
		room.WatchLevel = map[string]int{"L1100100": 4, "L1200100": 3, "L1300100": 4, "S1132210": 5}
		g.Channels["c1"] = room
		d.Guilds["g1"] = g
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
		t.Fatalf("expected only the alerts covered by this room's levels and the marine switch: %s", got)
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
	// Changing the sea-area delivery switch is not a KMA issue or cancellation.
	count := len(gw.posts)
	for _, marine := range []bool{true, false} {
		applyDoc(t, svc, func(d *config.Document) { d.DisasterAlerts.Marine = marine })
		tick(5 * time.Minute)
		if len(gw.posts) != count {
			t.Fatalf("switching marine to %v generated a weather event: %v", marine, gw.posts[count:])
		}
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

func TestDisasterRoomWatchLevels(t *testing.T) {
	svc := setupSvc(t)
	feed := &kmaFeed{}
	pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
	for ch, levels := range map[string]map[string]int{
		"quakes": {}, "mainland": {"광주·전라남도": 3, "L1052500": 1}, "islands": {"L1052500": 2},
	} {
		if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": ch, "respond": "mention", "watch_level": levels}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	applyDoc(t, svc, func(d *config.Document) {
		g := d.Guilds["g1"]
		g.Locale = "ko"
		d.Guilds["g1"] = g
	})
	gw := &postingGateway{}
	svc.Discord = gw
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tick := func() {
		now = now.Add(5 * time.Minute)
		svc.Clock = clock.Fixed{T: now}
		application.DisasterTickForTest(svc, context.Background())
	}
	tick()
	feed.quakes = []string{quake("202610010906", "일본 동쪽 해역", 35, 145, 7.1, "")}
	for id, name := range map[string]string{"L1052100": "목포시", "L1052500": "흑산도·홍도"} {
		feed.warnings = append(feed.warnings, warning(id, "광주·전라남도", name, "해일  ", "경보    ", "발표"), warning(id, "광주·전라남도", name, "지진해일", "주의", "발표"))
	}
	tick()
	got := strings.Join(gw.posts, "\n")
	if len(gw.posts) != 6 {
		t.Fatalf("expected three earthquake and three regional alerts, got %v", gw.posts)
	}
	for _, ch := range []string{"quakes", "mainland", "islands"} {
		if !strings.Contains(got, ch+"|🌏 국외 지진 발생|") {
			t.Errorf("missing overseas earthquake in %s: %s", ch, got)
		}
	}
	for _, line := range []string{"mainland|⚠️ 폭풍해일경보 발표|목포시\n", "mainland|⚠️ 지진해일주의보 발표|목포시\n", "islands|⚠️ 지진해일주의보 발표|흑산도·홍도\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("expected %q in %s", line, got)
		}
	}
	feed.warnings = nil
	tick()
	if len(gw.posts) != 9 || !strings.Contains(strings.Join(gw.posts[6:], "\n"), "islands|✅ 지진해일주의보 해제|흑산도·홍도") {
		t.Fatalf("lifts should reach the same three subscribers: %v", gw.posts[6:])
	}
	tick()
	if len(gw.posts) != 9 {
		t.Fatal("repeated an unchanged observation")
	}
}

func TestDisasterConfigureWatchLevels(t *testing.T) {
	svc := setupSvc(t)
	set := func(levels any) action.Result {
		return exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "watch_level": levels})
	}
	if r := set(map[string]int{"목포시": 3}); !r.OK {
		t.Fatal(r.Error)
	}
	for _, bad := range []any{map[string]int{"서울": 0}, map[string]int{"서울": 6}, map[string]int{" ": 2}, map[string]float64{"서울": 1.5}, nil, []string{"서울"}} {
		if set(bad).OK {
			t.Errorf("accepted invalid watch levels %v", bad)
		}
	}
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "disaster_alerts": "off"}); !r.OK || svc.Doc().Guilds["g"].Channels["c"].WatchLevel["목포시"] != 3 {
		t.Fatalf("room lost saved levels: %+v", r)
	}
	if r := set(map[string]int{}); !r.OK || len(svc.Doc().Guilds["g"].Channels["c"].WatchLevel) != 0 {
		t.Fatalf("could not clear room levels: %+v", r)
	}
}

func TestDisasterWarningUpgrades(t *testing.T) {
	svc := setupSvc(t)
	feed := &kmaFeed{}
	pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
	if r := exec(svc, "channels.configure", map[string]any{"id": "c1", "guild_id": "g1", "respond": "mention"}); !r.OK {
		t.Fatal(r.Error)
	}
	applyDoc(t, svc, func(d *config.Document) {
		g := d.Guilds["g1"]
		g.Locale = "ko"
		room := g.Channels["c1"]
		room.WatchLevel = map[string]int{"L1": 4, "L2": 3}
		g.Channels["c1"] = room
		d.Guilds["g1"] = g
	})
	gw := &postingGateway{}
	svc.Discord = gw
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	check := func(rows []string, want ...string) {
		t.Helper()
		feed.warnings = rows
		before := len(gw.posts)
		now = now.Add(5 * time.Minute)
		svc.Clock = clock.Fixed{T: now}
		application.DisasterTickForTest(svc, context.Background())
		got := gw.posts[before:]
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for _, prefix := range want {
			if !strings.Contains(strings.Join(got, "\n"), "c1|"+prefix) {
				t.Errorf("missing %q in %v", prefix, got)
			}
		}
	}
	// Simulate an installation upgrading with an existing visible heat advisory. The older build's set
	// never held the rain advisory, so the first check only records what is in force.
	legacy := `{"L1|H|2":{"RegionCode":"L1","Region":"경산시","Kind":"H","KindName":"폭염","Level":"2","LevelName":"주의보"}}`
	if _, err := svc.Store.DB.Exec(`INSERT INTO admin_state(k,v,updated_at) VALUES(?,?,?)`, "disaster:wrn_active", legacy, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	heatAdvisory := warning("L1", "경상북도", "경산시", "폭염", "주의", "발표")
	heatWarning := warning("L1", "경상북도", "경산시", "폭염  ", "경보    ", "변경")
	rainAdvisory := warning("L2", "경상북도", "포항시", "호우", "주의", "발표")
	check([]string{heatAdvisory, rainAdvisory})
	check([]string{heatWarning, rainAdvisory}, "⚠️ 폭염 주의보 → 경보 격상|경산시")
	check([]string{heatWarning, rainAdvisory})

	heatEmergency := warning("L1", "경상북도", "경산시", "폭염  ", "중대경보", "변경")
	rainWarning := warning("L2", "경상북도", "포항시", "호우", "경보", "대치")
	// The silent rain advisory upgrades too. An explicit release row for heat is not a second post.
	rows := []string{heatEmergency, warning("L1", "경상북도", "경산시", "폭염", "경보", "해제"), rainWarning}
	check(rows, "⚠️ 폭염 경보 → 중대경보 격상|경산시", "⚠️ 호우 주의보 → 경보 격상|포항시")
	check(rows)
	// A warning for the same phenomenon in another area is a new issue.
	otherHeat := warning("L2", "경상북도", "포항시", "폭염", "중대경보", "발표")
	check([]string{otherHeat, rainWarning}, "✅ 폭염중대경보 해제|경산시", "⚠️ 폭염중대경보 발표|포항시")
	// If both commands arrived between checks, their shared effective time identifies the upgrade.
	wind := warning("L1", "경상북도", "경산시", "강풍", "경보", "대치")
	rows = []string{otherHeat, rainWarning, warning("L1", "경상북도", "경산시", "강풍", "주의", "대치해제"), wind}
	check(rows, "⚠️ 강풍 주의보 → 경보 격상|경산시")
	check(rows)
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

// A check reads the rooms' levels and the instance switches from one configuration revision: a
// revision that turns marine on and drops the room's only sea area while KMA is answering must not
// combine the old room level with the new switch.
func TestDisasterTickReadsOneConfigRevision(t *testing.T) {
	const area = "S1252020"
	svc := setupSvc(t)
	var calls atomic.Int32
	fetching := make(chan struct{})
	proceed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "getEqkMsg") {
			_, _ = w.Write([]byte(`{"response":{"header":{"resultCode":"03","resultMsg":"NO_DATA"}}}`))
			return
		}
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte("[]"))
			return
		}
		close(fetching)
		select {
		case <-proceed:
			_, _ = w.Write([]byte("[" + warning(area, "제주도해상", "먼바다", "풍랑", "경보", "발표") + "]"))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	pointAt(t, svc, "kma", srv.URL, "KMA_TEST_KEY", "k")
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "a", "respond": "mention", "watch_level": map[string]int{area: 4}}); !r.OK {
		t.Fatal(r.Error)
	}
	gw := &postingGateway{}
	svc.Discord = gw
	now := time.Date(2026, 10, 1, 3, 10, 0, 0, time.UTC)
	svc.Clock = clock.Fixed{T: now}
	application.DisasterTickForTest(svc, context.Background())
	svc.Clock = clock.Fixed{T: now.Add(5 * time.Minute)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		application.DisasterTickForTest(svc, ctx)
		close(done)
	}()
	select {
	case <-fetching:
	case <-ctx.Done():
		t.Fatal("warning poll did not reach the test server")
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.DisasterAlerts.Marine = true
		g := d.Guilds["g1"]
		ch := g.Channels["a"]
		ch.WatchLevel = nil
		g.Channels["a"] = ch
		d.Guilds["g1"] = g
	})
	close(proceed)
	<-done
	if len(gw.posts) != 0 {
		t.Fatalf("no single config revision allows this card, got %q", gw.posts)
	}
}

// The first check after an upgrade from a build that stored a bare, filtered map announces nothing,
// whatever the rooms are set to: the old set left out advisories that may have been in force for
// hours. The set is then stored with its version, and a restart repeats nothing. The older build kept
// no record of which rooms it told (its own first check stored what was in force and sent nothing), so
// whatever is in force at the upgrade ends without a lift, even what it stored; a later change is news.
// The stored rows are what v1.4.0 wrote.
func TestDisasterOlderWarningStateIsAStartingPoint(t *testing.T) {
	const rain = `{"L1|R|3":{"RegionCode":"L1","Region":"경산시","ParentCode":"L1000000","Parent":"경상북도","Kind":"R","KindName":"호우","Level":"3","LevelName":"경보","Command":"발표","Announced":"202610011100","Effective":"202610011200"}}`
	const surge = `{"L2|해일|3":{"RegionCode":"L2","Region":"포항시","ParentCode":"L1000000","Parent":"경상북도","Kind":"해일","KindName":"해일","Level":"3","LevelName":"경보","Command":"발표","Announced":"202610011100","Effective":"202610011200"}}`
	for _, tc := range []struct {
		name, legacy string
		replaced     bool // the feed's warning took effect again after the stored one
	}{
		{"empty", `{}`, false},
		{"warning", rain, false},
		{"warning replaced since", rain, true},
		{"storm surge", surge, false},
		{"no version", `{"active":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := setupSvc(t)
			feed := &kmaFeed{}
			pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
			if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention", "watch_level": map[string]int{"L1": 5, "L2": 5}}); !r.OK {
				t.Fatal(r.Error)
			}
			applyDoc(t, svc, func(d *config.Document) {
				g := d.Guilds["g1"]
				g.Locale = "ko"
				d.Guilds["g1"] = g
			})
			now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
			if _, err := svc.Store.DB.Exec(`INSERT INTO admin_state(k,v,updated_at) VALUES(?,?,?)`, "disaster:wrn_active", tc.legacy, now.Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}
			rainNow := warning("L1", "경상북도", "경산시", "호우", "경보", "발표")
			if tc.replaced {
				rainNow = strings.Replace(rainNow, "202610011200", "202610011210", 1)
			}
			feed.mu.Lock()
			feed.warnings = []string{
				rainNow,
				warning("L1", "경상북도", "경산시", "강풍", "주의", "발표"),
				warning("L2", "경상북도", "포항시", "해일  ", "경보    ", "발표"),
			}
			feed.mu.Unlock()
			gw := &postingGateway{}
			svc.Discord = gw
			svc.Clock = clock.Fixed{T: now}
			application.DisasterTickForTest(svc, context.Background())
			if len(gw.posts) != 0 {
				t.Fatalf("the first check after the upgrade announced %q", gw.posts)
			}
			var n int
			if err := svc.Store.DB.QueryRow(`SELECT count(*) FROM admin_state WHERE k='disaster:wrn_active' AND json_extract(v,'$.version')=2 AND json_extract(v,'$.active."L2|O".Kind')='O' AND json_extract(v,'$.active."L1|W".Level')='2' AND json_extract(v,'$.sent') IS NULL`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("the set in force was not stored with its version and without deliveries: %d %v", n, err)
			}
			if err := svc.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := application.Open(svc.DataRoot, clock.Fixed{T: now.Add(5 * time.Minute)}, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			reopened.Discord = gw
			application.DisasterTickForTest(reopened, context.Background())
			if len(gw.posts) != 0 {
				t.Fatalf("repeated after a restart: %q", gw.posts)
			}
			// everything in force at the upgrade ends, and a new advisory is issued
			feed.mu.Lock()
			feed.warnings = []string{warning("L1", "경상북도", "경산시", "대설", "주의", "발표")}
			feed.mu.Unlock()
			reopened.Clock = clock.Fixed{T: now.Add(10 * time.Minute)}
			application.DisasterTickForTest(reopened, context.Background())
			if len(gw.posts) != 1 || !strings.HasPrefix(gw.posts[0], "c1|⚠️ 대설주의보 발표|경산시") {
				t.Fatalf("a change after the starting point is news, and nothing in force at the upgrade gets a lift: %q", gw.posts)
			}
		})
	}
}

// failingGateway is a Discord whose posts to one channel fail.
type failingGateway struct {
	postingGateway
	failing string
}

func (g *failingGateway) PostChannel(ctx context.Context, channelID, a, b string, card *action.Card) (string, error) {
	if channelID == g.failing {
		return "", fmt.Errorf("discord unavailable")
	}
	return g.postingGateway.PostChannel(ctx, channelID, a, b, card)
}

// A lift goes only to the rooms that were told the level is in force: not to a room that raised its
// level or got marine during the warning, not for a warning recorded silently as the starting point,
// not where the issue failed to post, and a room that watches only 경보 hears nothing when the
// advisory it never got ends.
func TestDisasterLiftsGoWhereTheIssueWent(t *testing.T) {
	const area, sea = "L1052100", "S1252020"
	rain := warning(area, "광주·전라남도", "목포시", "호우", "경보", "발표")
	rainAdvisory := warning(area, "광주·전라남도", "목포시", "호우", "주의", "대치")
	seaWarning := warning(sea, "제주도해상", "먼바다", "풍랑", "경보", "발표")
	setRoom := func(id string, levels map[string]int) func(*config.Document) {
		return func(d *config.Document) {
			g := d.Guilds["g1"]
			ch := g.Channels[id]
			ch.WatchLevel = levels
			g.Channels[id] = ch
			d.Guilds["g1"] = g
		}
	}
	for _, tc := range []struct {
		name   string
		rooms  map[string]map[string]int
		ticks  [][]string
		change func(*config.Document)
		fail   string
		want   [][]string // posts per tick after the first, as channel|title prefixes
	}{
		{name: "room raised during the warning", rooms: map[string]map[string]int{"a": {area: 3}, "b": {area: 2}},
			ticks: [][]string{nil, {rain}, nil}, change: setRoom("b", map[string]int{area: 3}),
			want: [][]string{{"a|⚠️ 호우경보 발표"}, {"a|✅ 호우경보 해제"}}},
		{name: "marine turned on during the warning", rooms: map[string]map[string]int{"a": {sea: 4}},
			ticks: [][]string{nil, {seaWarning}, nil}, change: func(d *config.Document) { d.DisasterAlerts.Marine = true },
			want: [][]string{{}, {}}},
		{name: "starting point", rooms: map[string]map[string]int{"a": {area: 3}},
			ticks: [][]string{{rain}, nil}, want: [][]string{{}}},
		{name: "issue failed to post", rooms: map[string]map[string]int{"a": {area: 3}, "b": {area: 3}},
			ticks: [][]string{nil, {rain}, nil}, fail: "b",
			want: [][]string{{"a|⚠️ 호우경보 발표"}, {"a|✅ 호우경보 해제"}}},
		{name: "warning drops to an advisory", rooms: map[string]map[string]int{"a": {area: 3}, "b": {area: 5}},
			ticks: [][]string{nil, {rain}, {rainAdvisory}, nil},
			want:  [][]string{{"a|⚠️ 호우경보 발표", "b|⚠️ 호우경보 발표"}, {"a|✅ 호우경보 해제", "b|✅ 호우경보 해제", "b|⚠️ 호우주의보 발표"}, {"b|✅ 호우주의보 해제"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := setupSvc(t)
			feed := &kmaFeed{}
			pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
			for id, levels := range tc.rooms {
				if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": id, "respond": "mention", "watch_level": levels}); !r.OK {
					t.Fatal(r.Error)
				}
			}
			applyDoc(t, svc, func(d *config.Document) {
				g := d.Guilds["g1"]
				g.Locale = "ko"
				d.Guilds["g1"] = g
			})
			gw := &failingGateway{failing: tc.fail}
			svc.Discord = gw
			now := time.Date(2026, 10, 1, 3, 10, 0, 0, time.UTC)
			for i, rows := range tc.ticks {
				if i == len(tc.ticks)-1 && tc.change != nil {
					applyDoc(t, svc, tc.change)
				}
				feed.mu.Lock()
				feed.warnings = rows
				feed.mu.Unlock()
				before := len(gw.posts)
				svc.Clock = clock.Fixed{T: now}
				now = now.Add(5 * time.Minute)
				application.DisasterTickForTest(svc, context.Background())
				if i == 0 {
					if len(gw.posts) != 0 {
						t.Fatalf("the first check announced %q", gw.posts)
					}
					continue
				}
				got := gw.posts[before:]
				want := tc.want[i-1]
				if len(got) != len(want) {
					t.Fatalf("check %d: got %q, want %q", i, got, want)
				}
				for _, prefix := range want {
					if !strings.Contains(strings.Join(got, "\n"), prefix+"|") {
						t.Fatalf("check %d: missing %q in %q", i, prefix, got)
					}
				}
			}
			var left int
			if err := svc.Store.DB.QueryRow(`SELECT count(*) FROM admin_state WHERE k='disaster:wrn_active' AND json_extract(v,'$.sent') IS NOT NULL`).Scan(&left); err != nil || left != 0 {
				t.Fatalf("delivery records outlived their warnings: %d %v", left, err)
			}
		})
	}
}

// storedDeliveries is the stored warning set's record of what each room was told, by area and kind.
func storedDeliveries(t *testing.T, svc *application.Service) map[string]map[string]string {
	t.Helper()
	var raw []byte
	if err := svc.Store.DB.QueryRow(`SELECT v FROM admin_state WHERE k='disaster:wrn_active'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state struct {
		Sent map[string]map[string]string `json:"sent"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state.Sent
}

// What each room was told survives a restart and follows the level in force. A level that ends takes
// its deliveries with it even where its lift failed to post, so when that level is issued again, its
// lift goes only where the new issue was posted. A room that no longer watches any area (earthquakes
// only) or was removed loses its deliveries at the next check and gets no lift.
func TestDisasterDeliveriesFollowTheLevelInForce(t *testing.T) {
	const area = "L1052100"
	for _, change := range []string{"restart", "lift failed, then issued again", "areas cleared", "room removed"} {
		t.Run(change, func(t *testing.T) {
			svc := setupSvc(t)
			feed := &kmaFeed{}
			pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
			for _, id := range []string{"a", "b"} {
				if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": id, "respond": "mention", "watch_level": map[string]int{area: 3}}); !r.OK {
					t.Fatal(r.Error)
				}
			}
			applyDoc(t, svc, func(d *config.Document) {
				g := d.Guilds["g1"]
				g.Locale = "ko"
				d.Guilds["g1"] = g
			})
			gw := &failingGateway{}
			svc.Discord = gw
			start := time.Date(2026, 10, 1, 3, 20, 0, 0, time.UTC)
			tick := func(svc *application.Service, minutes int, rows ...string) []string {
				feed.mu.Lock()
				feed.warnings = rows
				feed.mu.Unlock()
				before := len(gw.posts)
				svc.Clock = clock.Fixed{T: start.Add(time.Duration(minutes) * time.Minute)}
				application.DisasterTickForTest(svc, context.Background())
				return gw.posts[before:]
			}
			rain := warning(area, "광주·전라남도", "목포시", "호우", "경보", "발표")
			tick(svc, 0)
			if got := tick(svc, 5, rain); len(got) != 2 {
				t.Fatalf("issue: %q", got)
			}
			want := []string{"a|✅ 호우경보 해제|", "b|✅ 호우경보 해제|"}
			if change == "lift failed, then issued again" {
				gw.failing = "a"
				if got := tick(svc, 10, warning(area, "광주·전라남도", "목포시", "호우", "주의", "대치")); len(got) != 1 || !strings.HasPrefix(got[0], "b|✅ 호우경보 해제|") {
					t.Fatalf("drop to an advisory: %q", got)
				}
				if sent := storedDeliveries(t, svc); len(sent) != 0 {
					t.Fatalf("the ended level's deliveries outlived it: %v", sent)
				}
				rain = strings.Replace(strings.Replace(rain, "202610011100", "202610011230", 1), "202610011200", "202610011235", 1)
				if got := tick(svc, 15, rain); len(got) != 1 || !strings.HasPrefix(got[0], "b|⚠️ 호우 주의보 → 경보 격상|") {
					t.Fatalf("issued again: %q", got)
				}
				gw.failing = ""
				want = want[1:]
			}
			if change == "areas cleared" || change == "room removed" {
				if change == "areas cleared" {
					if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "a", "watch_level": map[string]int{}}); !r.OK {
						t.Fatal(r.Error)
					}
				} else {
					applyDoc(t, svc, func(d *config.Document) { delete(d.Guilds["g1"].Channels, "a") })
				}
				if got := tick(svc, 10, rain); len(got) != 0 {
					t.Fatalf("unchanged warnings: %q", got)
				}
				if sent := storedDeliveries(t, svc); len(sent[area+"|R"]) != 1 || sent[area+"|R"]["b"] != "3" {
					t.Fatalf("room a's delivery outlived its areas: %v", sent)
				}
				want = want[1:]
			}
			if err := svc.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := application.Open(svc.DataRoot, clock.Fixed{T: start.Add(20 * time.Minute)}, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			reopened.Discord = gw
			if got := tick(reopened, 20, rain); len(got) != 0 {
				t.Fatalf("repeated after a restart: %q", got)
			}
			got := tick(reopened, 25)
			if len(got) != len(want) {
				t.Fatalf("lift: got %q, want %q", got, want)
			}
			for i := range want {
				if !strings.HasPrefix(got[i], want[i]) {
					t.Fatalf("lift: got %q, want %q", got, want)
				}
			}
			if sent := storedDeliveries(t, reopened); len(sent) != 0 {
				t.Fatalf("deliveries outlived their warning: %v", sent)
			}
		})
	}
}

// One response can list an issue and the later release of the same level (both encodings: Korean
// command names in JSON, numeric codes in the typ01 text). The release ends the warning; an older
// release, a replacement by another level and an extension do not.
func TestDisasterReleaseInTheSameResponse(t *testing.T) {
	type row struct{ level, cmd, fc, ef string }
	json := func(r row) string {
		lvl := map[string]string{"2": "주의", "3": "경보"}[r.level]
		cmd := map[string]string{"1": "발표", "2": "대치", "3": "해제", "4": "대치해제", "5": "연장"}[r.cmd]
		return fmt.Sprintf(`{"REG_UP":"L1050000","REG_UP_KO":"광주·전라남도","REG_ID":"L1052100","REG_KO":"목포시","TM_FC":%q,"TM_EF":%q,"WRN":"호우","LVL":%q,"CMD":%q,"ED_TM":""}`, r.fc, r.ef, lvl, cmd)
	}
	text := func(r row) string {
		return strings.Join([]string{"L1050000", "광주·전라남도", "L1052100", "목포시", r.fc, r.ef, "R", r.level, r.cmd, "", ""}, ",")
	}
	issued := row{"3", "1", "202610011100", "202610011200"}
	for _, tc := range []struct {
		name string
		rows []row
		want []string
	}{
		{"later release", []row{issued, {"3", "3", "202610011250", "202610011300"}}, []string{"c1|✅ 호우경보 해제|목포시"}},
		{"release at the same time, announced later", []row{issued, {"3", "3", "202610011130", "202610011200"}}, []string{"c1|✅ 호우경보 해제|목포시"}},
		{"older release", []row{issued, {"3", "3", "202610011000", "202610011100"}}, nil},
		{"replaced by an advisory", []row{issued, {"3", "4", "202610011250", "202610011300"}, {"2", "2", "202610011250", "202610011300"}}, []string{"c1|✅ 호우경보 해제|목포시", "c1|⚠️ 호우주의보 발표|목포시"}},
		{"extended", []row{issued, {"3", "5", "202610011250", "202610011300"}}, nil},
	} {
		for _, encoding := range []string{"json", "text"} {
			t.Run(tc.name+"/"+encoding, func(t *testing.T) {
				svc := setupSvc(t)
				feed := &kmaFeed{}
				pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
				if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention", "watch_level": map[string]int{"목포시": 5}}); !r.OK {
					t.Fatal(r.Error)
				}
				applyDoc(t, svc, func(d *config.Document) {
					g := d.Guilds["g1"]
					g.Locale = "ko"
					d.Guilds["g1"] = g
				})
				gw := &postingGateway{}
				svc.Discord = gw
				now := time.Date(2026, 10, 1, 3, 10, 0, 0, time.UTC)
				serve := func(rows []row) {
					feed.mu.Lock()
					defer feed.mu.Unlock()
					feed.warnings, feed.text = nil, nil
					if encoding == "text" {
						feed.text = []string{}
					}
					for _, r := range rows {
						if encoding == "text" {
							feed.text = append(feed.text, text(r))
						} else {
							feed.warnings = append(feed.warnings, json(r))
						}
					}
				}
				tick := func(rows []row) []string {
					serve(rows)
					before := len(gw.posts)
					svc.Clock = clock.Fixed{T: now}
					now = now.Add(5 * time.Minute)
					application.DisasterTickForTest(svc, context.Background())
					return gw.posts[before:]
				}
				tick(nil)
				if got := tick([]row{issued}); len(got) != 1 || !strings.HasPrefix(got[0], "c1|⚠️ 호우경보 발표|목포시") {
					t.Fatalf("issue: %q", got)
				}
				got := tick(tc.rows)
				if len(got) != len(tc.want) {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
				for _, prefix := range tc.want {
					if !strings.Contains(strings.Join(got, "\n"), prefix) {
						t.Fatalf("missing %q in %q", prefix, got)
					}
				}
				if got := tick(tc.rows); len(got) != 0 {
					t.Fatalf("repeated: %q", got)
				}
			})
		}
	}
}

// A watch_level key must match a KMA area name or code exactly; the bot ships no KMA area list, so the
// room's watch_areas tell the console which keys match an area KMA has reported so far. A key that
// matches nothing (서울 is not an area name) is saved and flagged by its absence from "matched".
func TestDisasterWatchAreasMatchWhatKMAReported(t *testing.T) {
	svc := setupSvc(t)
	feed := &kmaFeed{}
	pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
	levels := map[string]int{"목포시": 3, "L1050000": 5, "서울": 5}
	configure := func() map[string]any {
		t.Helper()
		r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention", "watch_level": levels})
		if !r.OK {
			t.Fatal(r.Error)
		}
		areas, _ := r.Output["watch_areas"].(map[string]any)
		return areas
	}
	if areas := configure(); areas["checked"] != false || len(areas["matched"].(map[string]string)) != 0 {
		t.Fatalf("nothing can be checked before KMA reported any area: %v", areas)
	}
	gw := &postingGateway{}
	svc.Discord = gw
	feed.warnings = []string{warning("L1052100", "광주·전라남도", "목포시", "건조", "주의", "발표")}
	feed.warnings[0] = strings.Replace(feed.warnings[0], `"REG_UP":"L1000000"`, `"REG_UP":"L1050000"`, 1)
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 1, 3, 10, 0, 0, time.UTC)}
	application.DisasterTickForTest(svc, context.Background())
	areas := configure()
	matched := areas["matched"].(map[string]string)
	if areas["checked"] != true || matched["목포시"] != "목포시 (L1052100)" || matched["L1050000"] != "광주·전라남도 (L1050000)" || matched["서울"] != "" {
		t.Fatalf("watch areas after a KMA report: %v", areas)
	}
	list := out(t, svc, "channels.list", nil)
	for _, ch := range list["channels"].([]any) {
		c := ch.(map[string]any)
		if c["id"] == "c1" {
			if got := fmt.Sprint(c["watch_areas"]); !strings.Contains(got, "목포시:목포시 (L1052100)") || strings.Contains(got, "서울:") {
				t.Fatalf("channels.list watch_areas: %s", got)
			}
			return
		}
	}
	t.Fatal("room missing from channels.list")
}
