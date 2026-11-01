package application_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
)

// climbCard gives u1 (철수) a card item from two days of saying it in g/c.
func climbCard(t *testing.T, svc *application.Service, m *turnModel, eng *engine.Engine) {
	t.Helper()
	cardsOn(t, svc)
	m.extract = func(string) string { return climbFact }
	sayIn(eng, time.Now().Add(-23*time.Hour), said{"6001", "u1", "철수", "나 요즘 클라이밍 배우고 있어"}, said{"6101", "u2", "영희", "오 멋지다"})
	svc.RunRoomMemory(context.Background())
	sayIn(eng, time.Now().Add(-2*time.Hour), said{"6002", "u1", "철수", "클라이밍 배우는 거 재밌더라"}, said{"6102", "u2", "영희", "다음에 같이 가자"})
	svc.RunRoomMemory(context.Background())
}

func strict(t *testing.T, svc *application.Service, p config.GuildPrivacy) {
	t.Helper()
	applyDoc(t, svc, func(d *config.Document) {
		g := d.Guilds["g"]
		g.Privacy = p
		if g.Channels == nil {
			g.Channels = map[string]config.ChannelPolicy{}
		}
		g.Channels["c3"] = config.ChannelPolicy{Respond: "mention", Capture: true}
		d.Guilds["g"] = g
	})
}

// Off by default: another person of the conversation's card is shown, anywhere in the server.
// own_card_only keeps only the caller's; card_channel_only keeps an item where its evidence was said.
func TestStrictPrivacyCards(t *testing.T) {
	svc, m, eng := memorySvc(t)
	climbCard(t, svc, m, eng)
	turn(t, svc, "u2", "영희", "주말에 뭐 할까")
	if b := block(m.last("answer"), cardOpen, cardClose); !strings.Contains(b, "클라이밍") {
		t.Fatalf("default: the other speaker's card is shown:\n%s", m.last("answer"))
	}
	strict(t, svc, config.GuildPrivacy{OwnCardOnly: true})
	turn(t, svc, "u2", "영희", "주말에 뭐 할까")
	if strings.Contains(m.last("answer"), "클라이밍 배우는 중") {
		t.Fatal("own_card_only showed another person's card")
	}
	turn(t, svc, "u1", "철수", "주말에 뭐 할까")
	if b := block(m.last("answer"), cardOpen, cardClose); !strings.Contains(b, "클라이밍") {
		t.Fatal("own_card_only keeps the caller's own card")
	}
	strict(t, svc, config.GuildPrivacy{CardChannelOnly: true})
	turn(t, svc, "u1", "철수", "주말에 뭐 할까", func(in *application.ChatInput) { in.ChannelID = "c3" })
	if strings.Contains(m.last("answer"), cardOpen) {
		t.Fatal("card_channel_only used the card in another channel")
	}
	turn(t, svc, "u1", "철수", "주말에 뭐 할까")
	if !strings.Contains(m.last("answer"), cardOpen) {
		t.Fatal("card_channel_only keeps it where it was said")
	}
}

// "Don't analyze me" deletes the person's card, and no new one is made or used.
func TestProfileWithdrawalDeletesCards(t *testing.T) {
	svc, m, eng := memorySvc(t)
	climbCard(t, svc, m, eng)
	if _, c := mine(t, svc, inChannel("u1")); len(c) != 1 {
		t.Fatalf("card: %v", c)
	}
	if r := as(svc, "privacy.consent", map[string]any{"scope": "profile", "decision": "withdraw"}, inChannel("u1")); !r.OK {
		t.Fatalf("withdraw: %+v", r.Error)
	}
	svc.RunRoomMemory(context.Background())
	if items, c := mine(t, svc, inChannel("u1")); len(c) != 0 || len(items) == 0 {
		t.Fatalf("cards gone, room memory kept: %v %v", items, c)
	}
	turn(t, svc, "u1", "철수", "주말에 뭐 할까")
	if strings.Contains(m.last("answer"), cardOpen) {
		t.Fatal("a card for someone who opted out")
	}
}

// "Don't analyze me" while a promotion pass is between reading the consents and saving (here: during
// no_sensitive's classifier call) does not bring the deleted card back, then or on later passes.
func TestProfileWithdrawalDuringPromotion(t *testing.T) {
	svc, m, eng := memorySvc(t)
	strict(t, svc, config.GuildPrivacy{NoSensitive: true})
	var once sync.Once
	m.sensitive = func(string) string {
		once.Do(func() {
			if r := as(svc, "privacy.consent", map[string]any{"scope": "profile", "decision": "withdraw"}, inChannel("u1")); !r.OK {
				t.Errorf("withdraw: %+v", r.Error)
			}
		})
		return `{"sensitive":[]}`
	}
	climbCard(t, svc, m, eng)
	if m.calls("sensitive") == 0 {
		t.Fatal("the pass never reached the classifier")
	}
	if _, c := mine(t, svc, inChannel("u1")); len(c) != 0 {
		t.Fatalf("a card saved after the withdrawal: %v", c)
	}
	svc.RunRoomMemory(context.Background())
	if _, c := mine(t, svc, inChannel("u1")); len(c) != 0 {
		t.Fatalf("a card kept on a later pass: %v", c)
	}
}

// card_channel_only: an item is used only where all of its evidence was said. The same fact said on two
// days in #c and once more in #c3 merges evidence from both, so it is not shown in #c3.
func TestCardChannelOnlyNeedsAllEvidenceHere(t *testing.T) {
	svc, m, eng := memorySvc(t)
	strict(t, svc, config.GuildPrivacy{CardChannelOnly: true})
	climbCard(t, svc, m, eng)
	turn(t, svc, "u1", "철수", "주말에 뭐 할까")
	if !strings.Contains(m.last("answer"), cardOpen) {
		t.Fatal("the card is used where all its evidence was said")
	}
	at := time.Now().Add(-90 * time.Minute)
	for i, l := range []said{{"6003", "u1", "철수", "클라이밍 배우는 중인데 손가락 아파"}, {"6103", "u2", "영희", "그래도 계속 해"}} {
		_, _ = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c3", MessageID: l.id, AuthorID: l.user, AuthorName: l.name,
			Content: l.text, At: at.Add(time.Duration(i) * 30 * time.Second)})
	}
	svc.RunRoomMemory(context.Background())
	var inC3 int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM person_card_item p, json_each(p.evidence) e JOIN memory_observations o ON o.observation_id=e.value
		WHERE p.state='active' AND o.room='discord:g:c3'`).Scan(&inC3); err != nil || inC3 == 0 {
		t.Fatalf("the card item has no evidence from #c3 (%d, %v)", inC3, err)
	}
	turn(t, svc, "u1", "철수", "주말에 뭐 할까", func(in *application.ChatInput) { in.ChannelID = "c3" })
	if strings.Contains(m.last("answer"), cardOpen) {
		t.Fatal("card_channel_only used an item partly said in another channel")
	}
}

// no_hearsay also covers episode titles and live topics: a title or topic naming someone who said none of
// what is shown ("영희 이별") stays out, while it is shown by default.
func TestNoHearsayTitlesAndTopics(t *testing.T) {
	gossip := []said{{"8001", "u1", "철수", "영희 헤어졌대 위로 선물 사줄까"}, {"8002", "u2", "영희", "나 오늘 바빠"}}
	t.Run("episode title", func(t *testing.T) {
		svc, m, eng := memorySvc(t)
		m.extract = func(string) string {
			return `{"episodes":[{"op":"create","kind":"event","title":"{p2} 이별","items":[{"kind":"fact","text":"{p1}가 위로 선물을 사기로 함","by":["p1"],"ev":["m1"]}],"status":"done","importance":3}]}`
		}
		sayIn(eng, time.Now().Add(-2*time.Hour), gossip...)
		svc.RunRoomMemory(context.Background())
		filler(eng)
		turn(t, svc, "u3", "민수", "위로 선물 뭐 사지")
		if b := block(m.last("answer"), episodesOpen, episodesClose); !strings.Contains(b, "영희 이별") {
			t.Fatalf("default: the episode is shown with its title:\n%s", m.last("answer"))
		}
		strict(t, svc, config.GuildPrivacy{NoHearsay: true})
		turn(t, svc, "u3", "민수", "위로 선물 뭐 사지")
		if strings.Contains(m.last("answer"), "이별") {
			t.Fatalf("no_hearsay showed a title about someone who said none of it:\n%s", m.last("answer"))
		}
	})
	t.Run("topic", func(t *testing.T) {
		svc, m, eng := memorySvc(t)
		m.state = func(string) string {
			return `{"threads":[{"from":null,"label":"{p2} 이별","gist":"{p1}가 선물 고민","who":["p1","p2"],"status":"active","ev":["m1"]}]}`
		}
		// a room state needs six new lines and a quiet room
		sayIn(eng, time.Now().Add(-10*time.Minute), append(append([]said{}, gossip...), said{"8003", "u1", "철수", "그래도 선물은 사자"},
			said{"8004", "u2", "영희", "알았어"}, said{"8005", "u1", "철수", "뭐 살까"}, said{"8006", "u2", "영희", "케이크"})...)
		svc.RunRoomMemory(context.Background())
		turn(t, svc, "u3", "민수", "이별 선물 뭐가 좋아")
		if !strings.Contains(m.last("answer"), "영희 이별") {
			t.Fatalf("default: the topic is shown:\n%s", m.last("answer"))
		}
		strict(t, svc, config.GuildPrivacy{NoHearsay: true})
		turn(t, svc, "u3", "민수", "이별 선물 뭐가 좋아")
		if strings.Contains(m.last("answer"), "영희 이별") {
			t.Fatalf("no_hearsay showed a topic about someone who said none of its lines:\n%s", m.last("answer"))
		}
	})
}

// no_sensitive: a classifier decides which facts may become card items; a sensitive one does not, and a
// "remember this" the explicit classifier marks sensitive is refused.
func TestStrictPrivacyNoSensitive(t *testing.T) {
	svc, m, eng := memorySvc(t)
	strict(t, svc, config.GuildPrivacy{NoSensitive: true})
	m.sensitive = func(string) string { return `{"sensitive":["f1"]}` }
	climbCard(t, svc, m, eng)
	if m.calls("sensitive") == 0 {
		t.Fatal("the classifier was not asked")
	}
	if _, c := mine(t, svc, inChannel("u1")); len(c) != 0 {
		t.Fatalf("a sensitive fact became a card item: %v", c)
	}
	asked := m.calls("sensitive")
	m.sensitive = func(string) string { return `{"sensitive":[]}` }
	svc.RunRoomMemory(context.Background())
	if m.calls("sensitive") != asked {
		t.Fatal("a fact already classified is asked again")
	}
	sayIn(eng, time.Now().Add(-time.Minute), said{"7002", "u1", "철수", "기억해, 나 요즘 우울증 약 먹어"})
	m.explicit = func(string) string {
		return `{"intent":"remember","about":"self","fact":"{p1}는 우울증 약을 먹음","key":"medication","instruction":false,"sensitive":true}`
	}
	res := turn(t, svc, "u1", "철수", "기억해, 나 요즘 우울증 약 먹어", live("7002"))
	refused := false
	for _, tr := range res.Tools {
		refused = refused || tr.Tool == "memory.explicit" && tr.Data["reason"] == "sensitive"
	}
	if !refused {
		t.Fatalf("a sensitive remember request was kept: %+v", res.Tools)
	}
}

// The console's switches patch guilds.<g>.privacy.<key> one at a time: on sets true (even for a server
// with no entry yet), off unsets it.
func TestStrictPrivacyPatchedFromTheConsole(t *testing.T) {
	svc := setupSvc(t)
	patch := func(op map[string]any) {
		t.Helper()
		if r := exec(svc, "config.patch", map[string]any{"base_revision": svc.Revision(), "ops": []any{op}}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	patch(map[string]any{"path": []any{"guilds", "g9", "privacy", "no_hearsay"}, "value": true})
	patch(map[string]any{"path": []any{"guilds", "g9", "privacy", "card_channel_only"}, "value": true})
	if p := svc.Doc().Guilds["g9"].Privacy; !p.NoHearsay || !p.CardChannelOnly || p.OwnCardOnly || p.NoSensitive {
		t.Fatalf("on: %+v", p)
	}
	patch(map[string]any{"path": []any{"guilds", "g9", "privacy", "no_hearsay"}, "op": "unset"})
	if p := svc.Doc().Guilds["g9"].Privacy; p.NoHearsay || !p.CardChannelOnly {
		t.Fatalf("off: %+v", p)
	}
}
