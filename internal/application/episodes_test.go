package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/engine"
)

// roomModel is a fake summary model: it answers extraction and room-state prompts from functions the
// test sets, and remembers the prompts it was given.
type roomModel struct {
	mu      sync.Mutex
	extract func(prompt string) string
	state   func(prompt string) string
	prompts map[string][]string // kind (episode|state) → prompts
}

func (m *roomModel) calls(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts[kind])
}

func (m *roomModel) last(kind string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.prompts[kind]
	if len(p) == 0 {
		return ""
	}
	return p[len(p)-1]
}

func newRoomModel(t *testing.T) (*roomModel, string) {
	m := &roomModel{prompts: map[string][]string{},
		extract: func(string) string { return `{"episodes":[]}` },
		state:   func(string) string { return `{"threads":[]}` }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt := req.Messages[len(req.Messages)-1].Content
		kind, answer := "episode", ""
		m.mu.Lock()
		if strings.Contains(prompt, `"threads"`) {
			kind = "state"
		}
		m.prompts[kind] = append(m.prompts[kind], prompt)
		fn := m.extract
		if kind == "state" {
			fn = m.state
		}
		m.mu.Unlock()
		answer = fn(prompt)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(srv.Close)
	return m, srv.URL
}

func roomSvc(t *testing.T) (*application.Service, *roomModel, *engine.Engine) {
	t.Helper()
	m, url := newRoomModel(t)
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture = true
		d.Models["summary"] = d.Models["response"]
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}}
	})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	return svc, m, eng
}

type said struct{ id, user, name, text string }

func sayIn(eng *engine.Engine, base time.Time, lines ...said) {
	for i, l := range lines {
		_, _ = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: l.id, AuthorID: l.user, AuthorName: l.name,
			Content: l.text, At: base.Add(time.Duration(i) * 30 * time.Second)})
	}
}

var hike = []said{
	{"3001", "u1", "철수", "토요일에 등산 갈래?"},
	{"3002", "u2", "영희", "좋아 토요일 등산 가자"},
	{"3003", "u1", "철수", "그럼 토요일 아침 9시 등산 확정"},
	{"3004", "u2", "영희", "등산 김밥은 내가 싸갈게"},
}

const hikeAnswer = `{"episodes":[{"op":"create","link_to":null,"kind":"plan","title":"토요일 등산","items":[
 {"kind":"proposal","text":"{p1}가 토요일 등산 제안","by":["p1"],"ev":["m1"]},
 {"kind":"decision","text":"토요일 아침 9시 등산 확정","by":["p1","p2"],"ev":["m2","m3"]},
 {"kind":"fact","text":"{p2}가 김밥 담당","by":["p2"],"ev":["m4"]}],
 "status":"decided","importance":3}]}`

func episodeList(t *testing.T, svc *application.Service) []map[string]any {
	t.Helper()
	r := exec(svc, "memory.episodes.list", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !r.OK {
		t.Fatalf("list: %+v", r.Error)
	}
	return jsonList(r.Output["episodes"])
}

func jsonList(v any) []map[string]any {
	raw, _ := json.Marshal(v)
	var out []map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func showItems(t *testing.T, svc *application.Service, id string) (map[string]any, []map[string]any) {
	t.Helper()
	r := exec(svc, "memory.episodes.show", map[string]any{"id": id})
	if !r.OK {
		t.Fatalf("show: %+v", r.Error)
	}
	return jsonShape(r.Output["episode"]), jsonList(r.Output["items"])
}

func itemState(items []map[string]any, kind string) string {
	for _, it := range items {
		if it["kind"] == kind {
			return it["state"].(string)
		}
	}
	return "missing"
}

func drain(t *testing.T, svc *application.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := svc.DrainRedactions(ctx); err != nil {
		t.Fatal(err)
	}
}

// A closed conversation becomes one episode with its evidence; the next pass does not ask again; the
// list carries no message text while show reads the cited lines live; a deletion hides an item, an
// edit makes one dirty, and losing 40% of the items queues the episode for regeneration.
func TestRoomMemoryExtractsOnceAndInvalidates(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return hikeAnswer }
	sayIn(eng, time.Now().Add(-2*time.Hour), hike...)

	rep := svc.RunRoomMemory(context.Background())
	if rep.Segments != 1 || rep.Episodes != 1 || m.calls("episode") != 1 {
		t.Fatalf("first pass: %+v calls=%d", rep, m.calls("episode"))
	}
	if p := m.last("episode"); !strings.Contains(p, "[m1] 철수(p1): 토요일에 등산 갈래?") || strings.Contains(p, "u1") {
		t.Fatalf("the prompt numbers lines and hides accounts:\n%s", p)
	}
	if rep := svc.RunRoomMemory(context.Background()); rep.Segments != 0 || m.calls("episode") != 1 {
		t.Fatalf("an extracted segment is not asked about again: %+v", rep)
	}
	list := episodeList(t, svc)
	if len(list) != 1 || list[0]["status"] != "decided" || list[0]["items"].(float64) != 3 || list[0]["participants"].(float64) != 2 || list[0]["source"] != "discord" {
		t.Fatalf("list: %+v", list)
	}
	raw, _ := json.Marshal(list)
	for _, l := range hike {
		if strings.Contains(string(raw), l.text) {
			t.Fatal("the list carries message text")
		}
	}
	id := list[0]["id"].(string)
	ep, items := showItems(t, svc, id)
	if len(items) != 3 || items[0]["text"] != "철수가 토요일 등산 제안" {
		t.Fatalf("show renders people by their names: %+v", items)
	}
	if ev := jsonList(items[1]["evidence"]); len(ev) != 2 || ev[1]["text"] != hike[2].text {
		t.Fatalf("show reads the cited lines: %+v", ev)
	}
	if people := jsonList(ep["participants"]); len(people) != 2 || people[0]["label"] != "철수" || strings.Contains(people[0]["ref"].(string), "u1") {
		t.Fatalf("participants: %+v", people)
	}

	// a deleted message hides the item citing it (1 of 3: kept)
	if err := svc.MessagesDeleted("g", "c", []string{"3004"}); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	_, items = showItems(t, svc, id)
	if itemState(items, "fact") != "hidden" || itemState(items, "proposal") != "active" {
		t.Fatalf("after the deletion: %+v", items)
	}
	if ev := jsonList(items[2]["evidence"]); ev[0]["gone"] != true || ev[0]["text"] != nil {
		t.Fatalf("a deleted line shows no text: %+v", ev)
	}
	if l := episodeList(t, svc); l[0]["regen"] != false || l[0]["hidden"].(float64) != 1 {
		t.Fatalf("one hidden item of three is not a regeneration: %+v", l)
	}
	// an edit marks the item dirty: 2 of 3 gone → regeneration
	if err := svc.MessageEdited("g", "c", "3001", "일요일에 등산 갈래?", time.Now()); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	_, items = showItems(t, svc, id)
	if itemState(items, "proposal") != "dirty" {
		t.Fatalf("after the edit: %+v", items)
	}
	if l := episodeList(t, svc); l[0]["regen"] != true {
		t.Fatalf("40%% hidden queues a regeneration: %+v", l)
	}
	// the regeneration reads the edited text and saves (an edited message is current again)
	m.extract = func(prompt string) string {
		if !strings.Contains(prompt, "일요일에 등산 갈래?") || strings.Contains(prompt, hike[3].text) {
			t.Errorf("regeneration prompt:\n%s", prompt)
		}
		return `{"episodes":[{"op":"create","kind":"plan","title":"등산","items":[
			{"kind":"proposal","text":"{p1}가 일요일 등산 제안","by":["p1"],"ev":["m1"]}],"status":"open"}]}`
	}
	if rep := svc.RunRoomMemory(context.Background()); rep.Regenerated != 1 {
		t.Fatalf("regeneration after an edit: %+v", rep)
	}
	if l := episodeList(t, svc); len(l) != 1 || l[0]["id"] == id || l[0]["regen"] != false {
		t.Fatalf("after the regeneration: %+v", l)
	}
}

// A hidden decision queues the episode on its own; the regeneration makes it again from the messages
// that remain and replaces the old one.
func TestRoomMemoryRegeneratesWhenADecisionIsHidden(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return hikeAnswer }
	sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
	svc.RunRoomMemory(context.Background())
	old := episodeList(t, svc)[0]["id"].(string)

	if err := svc.MessagesDeleted("g", "c", []string{"3003"}); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	if l := episodeList(t, svc); l[0]["regen"] != true || l[0]["hidden"].(float64) != 1 {
		t.Fatalf("a hidden decision queues a regeneration: %+v", l)
	}
	m.extract = func(prompt string) string {
		if strings.Contains(prompt, "확정") {
			t.Error("the deleted message reached the regeneration prompt")
		}
		return `{"episodes":[{"op":"create","kind":"plan","title":"토요일 등산","items":[
			{"kind":"proposal","text":"{p1}가 토요일 등산 제안","by":["p1"],"ev":["m1"]},
			{"kind":"fact","text":"{p2}가 김밥 담당","by":["p2"],"ev":["m3"]}],"status":"open"}]}`
	}
	rep := svc.RunRoomMemory(context.Background())
	if rep.Regenerated != 1 {
		t.Fatalf("regeneration: %+v", rep)
	}
	l := episodeList(t, svc)
	if len(l) != 1 || l[0]["id"] == old || l[0]["regen"] != false || l[0]["items"].(float64) != 2 || l[0]["status"] != "open" {
		t.Fatalf("the regenerated episode replaces the old one: %+v", l)
	}
}

// Withdrawn consent deletes the person's items (their messages and the ones naming them) and takes
// them off the participants; the room state citing them goes too.
func TestRoomMemoryWithdrawalDeletesThePersonsItems(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return hikeAnswer }
	m.state = func(string) string {
		return `{"threads":[{"label":"토요일 등산","gist":"{p1}와 {p2}가 9시로 정함","who":["p1","p2"],"status":"active","ev":["m2","m3"]}],"open_questions":[],"mood":null,"banter":null}`
	}
	sayIn(eng, time.Now().Add(-2*time.Hour), append(append([]said{}, hike...), said{"3005", "u1", "철수", "등산화 챙겨"})...)
	svc.RunRoomMemory(context.Background())
	if r := exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["state"] == nil {
		t.Fatalf("room state: %+v", r)
	}
	id := episodeList(t, svc)[0]["id"].(string)

	if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u2")); !r.OK {
		t.Fatalf("withdraw: %+v", r.Error)
	}
	drain(t, svc)
	ep, items := showItems(t, svc, id)
	if len(items) != 1 || items[0]["kind"] != "proposal" {
		t.Fatalf("only the item without the person stays: %+v", items)
	}
	if people := jsonList(ep["participants"]); len(people) != 1 || people[0]["label"] != "철수" {
		t.Fatalf("participants after the withdrawal: %+v", people)
	}
	if l := episodeList(t, svc); l[0]["regen"] != true {
		t.Fatalf("a deleted decision queues a regeneration: %+v", l)
	}
	if r := exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["state"] != nil {
		t.Fatalf("the room state citing the person is deleted: %+v", r)
	}
}

// The room state waits for the room to go quiet with enough new messages, then updates incrementally
// from what is new with the previous state in the prompt.
func TestRoomStateDebouncesAndUpdatesIncrementally(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.state = func(string) string {
		return `{"threads":[{"label":"등산","gist":"{p1}가 꺼낸 토요일 등산 얘기","who":["p1"],"status":"active","ev":["m1"]}]}`
	}
	now := time.Now()
	sayIn(eng, now.Add(-20*time.Minute), hike...)
	svc.RunRoomMemory(context.Background())
	if m.calls("state") != 0 {
		t.Fatal("four messages are not enough for a room state")
	}
	sayIn(eng, now.Add(-10*time.Minute), said{"3005", "u1", "철수", "등산화 챙겨"}, said{"3006", "u2", "영희", "물도 챙기자"})
	svc.RunRoomMemory(context.Background())
	if m.calls("state") != 1 {
		t.Fatalf("a quiet room with six new messages gets a state: %d", m.calls("state"))
	}
	if p := m.last("state"); !strings.Contains(p, "등산화 챙겨") || strings.Contains(p, "[t1]") {
		t.Fatalf("the first state is made from the messages alone:\n%s", p)
	}
	r := exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"})
	st := jsonShape(r.Output["state"])
	if th := jsonList(st["threads"]); len(th) != 1 || th[0]["label"] != "등산" || th[0]["gist"] != "철수가 꺼낸 토요일 등산 얘기" || jsonShape(r.Output)["updated_n"].(float64) != 1 {
		t.Fatalf("state: %+v", r.Output)
	}
	svc.RunRoomMemory(context.Background())
	if m.calls("state") != 1 {
		t.Fatal("nothing new: no state call")
	}
	var more []said
	for i := 0; i < 5; i++ {
		more = append(more, said{strconv.Itoa(3100 + i), "u2", "영희", "점심은 산 아래 식당 " + strconv.Itoa(i)})
	}
	sayIn(eng, now.Add(-5*time.Minute), more...)
	svc.RunRoomMemory(context.Background())
	if m.calls("state") != 2 {
		t.Fatalf("five new messages in a quiet room update the state: %d", m.calls("state"))
	}
	if p := m.last("state"); !strings.Contains(p, "[t1] 등산") || strings.Contains(p, "등산화 챙겨") {
		t.Fatalf("an incremental update shows the previous state and only what is new:\n%s", p)
	}
	// a deletion of a message the state cites deletes the state
	if err := svc.MessagesDeleted("g", "c", []string{"3100"}); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	if r := exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["state"] != nil {
		t.Fatalf("state after deleting its evidence: %+v", r.Output)
	}
}

// The derived budget stops the room state first (at 80%), then extraction; 0 switches room memory off.
func TestRoomMemoryBudgetStopsStateFirst(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return hikeAnswer }
	sayIn(eng, time.Now().Add(-2*time.Hour), append(append([]said{}, hike...), said{"3005", "u1", "철수", "등산화 챙겨"})...)
	applyDoc(t, svc, func(d *config.Document) { d.Memory.DerivedBudgetUSD = "0" })
	if rep := svc.RunRoomMemory(context.Background()); rep.Segments != 0 || m.calls("episode")+m.calls("state") != 0 {
		t.Fatalf("a zero budget is off: %+v", rep)
	}
	// one call costs 0.00001: extraction fits under 0.000012, the state's 80% share does not
	applyDoc(t, svc, func(d *config.Document) { d.Memory.DerivedBudgetUSD = "0.000012" })
	rep := svc.RunRoomMemory(context.Background())
	if rep.Episodes != 1 || m.calls("state") != 0 || rep.StoppedBy != "budget" {
		t.Fatalf("budget order: %+v state calls=%d", rep, m.calls("state"))
	}
	if r := exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["state"] != nil {
		t.Fatal("no state past the state's share")
	}
	// the list reports this month's derived spend against the budget
	bud := jsonShape(exec(svc, "memory.episodes.list", map[string]any{"guild_id": "g", "channel_id": "c"}).Output["budget"])
	if bud["limit_usd"].(float64) != 0.000012 || bud["spent_usd"].(float64) <= 0 {
		t.Fatalf("budget in the list: %+v", bud)
	}
}

// A later conversation updates an open plan through the link the prompt offers; a plan past its date
// expires in code without a model call.
func TestRoomMemoryLinksUpdatesAndExpires(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string {
		return `{"episodes":[{"op":"create","kind":"plan","title":"토요일 등산","items":[
			{"kind":"proposal","text":"{p1}가 토요일 등산 제안","by":["p1"],"ev":["m1"]}],"status":"open"}]}`
	}
	sayIn(eng, time.Now().Add(-5*time.Hour), hike[:2]...)
	if rep := svc.RunRoomMemory(context.Background()); rep.Episodes != 1 {
		t.Fatalf("first: %+v", rep)
	}
	id := episodeList(t, svc)[0]["id"].(string)
	due := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
	m.extract = func(prompt string) string {
		if !strings.Contains(prompt, "[e1] 토요일 등산 (open") {
			t.Errorf("the open plan is offered:\n%s", prompt)
		}
		return `{"episodes":[{"op":"update","link_to":"e1","items":[
			{"kind":"decision","text":"9시 등산 확정","by":["p1","p2"],"ev":["m1","m2"]}],"status":"decided","due":"` + due + `"}]}`
	}
	sayIn(eng, time.Now().Add(-2*time.Hour), hike[2:]...)
	if rep := svc.RunRoomMemory(context.Background()); rep.Episodes != 1 || rep.Segments != 1 {
		t.Fatalf("update: %+v", rep)
	}
	l := episodeList(t, svc)
	if len(l) != 1 || l[0]["id"] != id || l[0]["status"] != "decided" || l[0]["items"].(float64) != 2 || l[0]["participants"].(float64) != 2 {
		t.Fatalf("after the update: %+v", l)
	}
	calls := m.calls("episode")
	if rep := svc.RunRoomMemory(context.Background()); rep.Expired != 1 || m.calls("episode") != calls {
		t.Fatalf("expiry: %+v", rep)
	}
	if l := episodeList(t, svc); l[0]["status"] != "expired" {
		t.Fatalf("after expiry: %+v", l)
	}
}

// A Kakao block is estimated first, then extracted on request with speakers as kakao:<room>:<label>.
func TestKakaoBlockExtraction(t *testing.T) {
	svc, m, _ := roomSvc(t)
	csv := writeKakao(t, svc.DataRoot, "club.csv", 40)
	if r := exec(svc, "imports.start", map[string]any{"source": "kakao_csv", "path": csv, "room": "kakao:club", "label": "동아리"}); !r.OK {
		t.Fatalf("import %+v", r.Error)
	}
	est := exec(svc, "memory.episodes.extract", map[string]any{"source_room": "kakao:club", "dry_run": true})
	if !est.OK || est.Output["messages"] != 40 || est.Output["segments"].(int) == 0 || est.Output["est_input_tokens"].(int) == 0 || m.calls("episode") != 0 {
		t.Fatalf("estimate: %+v %+v", est.Output, est.Error)
	}
	m.extract = func(string) string {
		return `{"episodes":[{"op":"create","kind":"event","title":"동아리 회의","items":[{"kind":"moment","text":"{p1}가 회의를 시작함","by":["p1"],"ev":["m1"]}]}]}`
	}
	r := exec(svc, "memory.episodes.extract", map[string]any{"source_room": "kakao:club", "run_now": true, "limit": 50, "budget_usd": "1"})
	if !r.OK {
		t.Fatalf("extract: %+v", r.Error)
	}
	job := jsonShape(r.Output["job"])
	if job["done"] != true || job["episodes"].(float64) == 0 || job["episodes"].(float64) != float64(m.calls("episode")) {
		t.Fatalf("job: %+v calls=%d", job, m.calls("episode"))
	}
	l := exec(svc, "memory.episodes.list", map[string]any{"room": "kakao:club"})
	list := jsonList(l.Output["episodes"])
	if len(list) == 0 || !strings.HasPrefix(list[0]["source"].(string), "kakao:") || list[0]["guild_id"] != "" {
		t.Fatalf("kakao episodes: %+v", list)
	}
	ep, _ := showItems(t, svc, list[0]["id"].(string))
	people := jsonList(ep["participants"])
	if len(people) != 1 || !strings.HasPrefix(people[0]["ref"].(string), "kakao:club:") {
		t.Fatalf("kakao speakers stay kakao:<room>:<label>: %+v", people)
	}
	// forgetting the room takes its episodes
	if f := exec(svc, "memory.episodes.forget", map[string]any{"room": "kakao:club"}); !f.OK || f.Output["deleted"].(int) != len(list) {
		t.Fatalf("forget: %+v", f)
	}
}

// The room memory actions are for the console: a person speaking in a channel is refused, and the state
// shown carries no person references or message IDs.
func TestRoomMemoryActionsAreConsoleOnly(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.state = func(string) string {
		return `{"threads":[{"label":"등산","gist":"{p1}가 꺼낸 토요일 등산 얘기","who":["p1"],"status":"active","ev":["m1"]}]}`
	}
	now := time.Now()
	sayIn(eng, now.Add(-20*time.Minute), hike...)
	svc.RunRoomMemory(context.Background())
	sayIn(eng, now.Add(-10*time.Minute), said{"3005", "u1", "철수", "등산화 챙겨"}, said{"3006", "u2", "영희", "물도 챙기자"})
	svc.RunRoomMemory(context.Background())
	for _, act := range []string{"memory.episodes.list", "memory.episodes.show", "memory.episodes.forget", "memory.state.show", "memory.episodes.extract"} {
		for _, inv := range []*action.Invoker{inChannel("u1"), {Via: "command", UserID: "operator"}} {
			if r := as(svc, act, map[string]any{"guild_id": "g", "channel_id": "c", "id": "x"}, inv); r.OK || r.Error == nil || r.Error.Code != errcode.PermissionDenied {
				t.Fatalf("%s for a person in a channel: %+v", act, r)
			}
		}
	}
	r := exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"})
	raw, _ := json.Marshal(r.Output["state"])
	st := jsonShape(r.Output["state"])
	if th := jsonList(st["threads"]); len(th) != 1 || th[0]["label"] != "등산" || th[0]["last_at"] == "" {
		t.Fatalf("state: %s", raw)
	}
	for _, key := range []string{`"who":["`, `"ev":["`, `"last":"m`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("the state shows %s: %s", key, raw)
		}
	}
}

// The last answer keeps what validated: an episode with one item citing a line that is not in the input
// is saved without that item, and a state whose banter has no evidence is saved without the banter. A
// state with nothing valid left is still refused (it would erase the room's topics).
func TestRoomMemoryKeepsTheValidPartOfTheLastAnswer(t *testing.T) {
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string {
		return strings.Replace(hikeAnswer, `"ev":["m4"]}]`, `"ev":["m4"]},{"kind":"fact","text":"{p1}가 등산화 삼","by":["p1"],"ev":["m99"]}]`, 1)
	}
	sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
	if rep := svc.RunRoomMemory(context.Background()); rep.Episodes != 1 || m.calls("episode") != 2 {
		t.Fatalf("asked again, then kept the valid part: %+v calls=%d", rep, m.calls("episode"))
	}
	if list := episodeList(t, svc); len(list) != 1 || list[0]["items"].(float64) != 3 {
		t.Fatalf("the three valid items are kept: %+v", list)
	}

	svc, m, eng = roomSvc(t)
	thread := `{"label":"등산","gist":"{p1}가 꺼낸 토요일 등산 얘기","who":["p1"],"status":"active","ev":["m1"]}`
	m.state = func(string) string { return `{"threads":[` + thread + `],"banter":{"text":"서로 놀림","ev":[]}}` }
	sayIn(eng, time.Now().Add(-20*time.Minute), append(hike, said{"3005", "u1", "철수", "등산화 챙겨"}, said{"3006", "u2", "영희", "물도 챙기자"})...)
	svc.RunRoomMemory(context.Background())
	st := jsonShape(exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"}).Output["state"])
	if m.calls("state") != 2 || len(jsonList(st["threads"])) != 1 || st["banter"] != nil {
		t.Fatalf("calls=%d state=%+v", m.calls("state"), st)
	}

	svc, m, eng = roomSvc(t)
	m.state = func(string) string {
		return `{"threads":[{"label":"등산","who":["p1"],"status":"active","ev":["m99"]}]}`
	}
	sayIn(eng, time.Now().Add(-20*time.Minute), append(hike, said{"3005", "u1", "철수", "등산화 챙겨"}, said{"3006", "u2", "영희", "물도 챙기자"})...)
	svc.RunRoomMemory(context.Background())
	if r := exec(svc, "memory.state.show", map[string]any{"guild_id": "g", "channel_id": "c"}); m.calls("state") != 2 || r.Output["state"] != nil {
		t.Fatalf("nothing valid: no state (calls=%d): %+v", m.calls("state"), r.Output)
	}
}
