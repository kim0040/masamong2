package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/memory/engine"
)

func asUser(svc *application.Service, user, act string, in map[string]any) action.Result {
	return as(svc, act, in, &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: user})
}

func TestConsentNeedsTheButtonAndOnlyThePersonCanAgree(t *testing.T) {
	svc := setupSvc(t)
	if svc.ConsentAllows("u1", "fortune") {
		t.Fatal("fortune needs consent")
	}
	// asking to agree shows the policy and the button, and records nothing
	r := asUser(svc, "u1", "privacy.consent", map[string]any{"scope": "운세", "decision": "grant"})
	card, ok := r.Output["card"].(action.Card)
	if !r.OK || !ok || len(card.Buttons) != 1 || card.Buttons[0].ID != "privacy:grant:fortune" || !strings.Contains(card.Text, "생년월일") && !strings.Contains(card.Text, "birth") {
		t.Fatalf("notice: %+v", r)
	}
	if svc.ConsentAllows("u1", "fortune") {
		t.Fatal("reading the notice is not agreeing")
	}
	// the button press
	if r := asUser(svc, "u1", "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}); !r.OK {
		t.Fatalf("grant: %+v", r)
	}
	if !svc.ConsentAllows("u1", "fortune") || svc.ConsentAllows("u2", "fortune") {
		t.Fatal("consent is per person")
	}
	// someone cannot agree for someone else: the invoker is always the subject
	_ = asUser(svc, "u3", "privacy.consent", map[string]any{"user_id": "u2", "scope": "fortune", "decision": "grant", "confirmed": true})
	if svc.ConsentAllows("u2", "fortune") || !svc.ConsentAllows("u3", "fortune") {
		t.Fatal("user_id from Discord must be ignored")
	}
	// the console records withdrawals but never grants
	if r := exec(svc, "privacy.consent", map[string]any{"user_id": "u2", "scope": "fortune", "decision": "grant", "confirmed": true}); r.OK {
		t.Fatal("the console must not grant")
	}
	if r := exec(svc, "privacy.consent", map[string]any{"user_id": "u1", "scope": "fortune", "decision": "withdraw"}); !r.OK || r.Output["kept_data"] != true {
		t.Fatalf("console withdraw: %+v", r)
	}
	if svc.ConsentAllows("u1", "fortune") {
		t.Fatal("withdrawal applies at once")
	}
	list := exec(svc, "privacy.consent", nil)
	if n := len(list.Output["records"].([]map[string]any)); n != 2 {
		t.Fatalf("records: %d", n)
	}
	if r := exec(svc, "privacy.consent", map[string]any{"user_id": "u1", "scope": "주민번호", "decision": "withdraw"}); r.OK {
		t.Fatal("unknown scope")
	}
}

func TestWithdrawingMemoryForgetsThePerson(t *testing.T) {
	svc := setupSvc(t)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for i, who := range []string{"u1", "u2", "u1"} {
		if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m" + string(rune('0'+i)), AuthorID: who,
			AuthorName: who, Content: "말 " + who, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if !svc.MemoryAllowed("u1") {
		t.Fatal("memory applies until withdrawn")
	}
	r := asUser(svc, "u1", "privacy.consent", map[string]any{"scope": "기억", "decision": "withdraw"})
	if !r.OK || r.Output["removed_messages"] != 2 {
		t.Fatalf("withdraw memory: %+v", r)
	}
	if svc.MemoryAllowed("u1") || !svc.MemoryAllowed("u2") {
		t.Fatal("only u1 is out")
	}
	msgs, _ := eng.Store.RecentMessages(engine.RoomKey("g", "c"), 10)
	if len(msgs) != 1 || msgs[0].ActorAccount != "u2" {
		t.Fatalf("u1's messages must be gone: %+v", msgs)
	}
	st := asUser(svc, "u1", "privacy.consent", nil)
	card := st.Output["card"].(action.Card)
	if len(card.Fields) != 6 { // memory, fortune, school and transfer notices, participation, profile
		t.Fatalf("status card: %+v", card)
	}
}

// LIVE-06: a withdrawn opt-out scope shows the button that turns it back on.
func TestWithdrawnMemoryConsentCanBeTurnedBackOn(t *testing.T) {
	svc := setupSvc(t)
	if r := asUser(svc, "u1", "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}); !r.OK || svc.MemoryAllowed("u1") {
		t.Fatalf("withdraw: %+v", r)
	}
	r := asUser(svc, "u1", "privacy.consent", map[string]any{"scope": "memory", "decision": "grant"})
	card, ok := r.Output["card"].(action.Card)
	if !ok || len(card.Buttons) != 1 || card.Buttons[0].ID != "privacy:grant:memory" {
		t.Fatalf("no way back: %+v", r)
	}
	if r := asUser(svc, "u1", "privacy.consent", map[string]any{"scope": "memory", "decision": "grant", "confirmed": true}); !r.OK || !svc.MemoryAllowed("u1") {
		t.Fatalf("grant: %+v", r)
	}
}
