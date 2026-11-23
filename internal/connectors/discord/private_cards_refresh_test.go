package discord

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/memory/store"
)

// forgetButtonIDs are the custom IDs of the forget buttons in a message Discord was sent (a post or an
// interaction edit).
func forgetButtonIDs(t *testing.T, body string) []string {
	t.Helper()
	var msg struct {
		Components []struct {
			Components []struct {
				CustomID string `json:"custom_id"`
			} `json:"components"`
		} `json:"components"`
	}
	if i := strings.Index(body, "{"); i >= 0 {
		_ = json.NewDecoder(strings.NewReader(body[i:])).Decode(&msg)
	}
	var out []string
	for _, row := range msg.Components {
		for _, b := range row.Components {
			if strings.HasPrefix(b.CustomID, "mem:forget:") {
				out = append(out, b.CustomID)
			}
		}
	}
	return out
}

// The forget button of a card sent to a DM keeps its server when it is refreshed: memory changed before the
// press, so the Action answers with a fresh preview, and the edited card's new button still names the
// server (pressed in a DM, an interaction names none). Pressing it then forgets everything of that server,
// and only of that server.
func TestRefreshedDMForgetButtonKeepsItsServer(t *testing.T) {
	g, fake := privateCardGateway(t)
	ref := store.PersonRef("u1")
	card := func(id, guild string) {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := g.svc.Store.DB.Exec(`INSERT INTO person_card_item(id,guild_id,person_ref,key,text,evidence,episodes,origin,state,created_at,updated_at)
			VALUES(?,?,?,?,?,'[]','[]','test','active',?,?)`, id, guild, ref, "k-"+id, "a synthetic card line "+id, now, now); err != nil {
			t.Fatal(err)
		}
	}
	card("first", "g1")
	card("control", "g2")
	cmd, _ := ByName(shippedCatalog(), "forgetme")
	g.runCommand(g.sess, roomMessage("m1", "!"+cmd.Aliases[0]), cmd, nil)
	g.inflight.Wait()
	dm, ok := fake.find(http.MethodPost, "/channels/1/messages")
	if !ok {
		t.Fatalf("no DM card: %+v", fake.calls)
	}
	ids := forgetButtonIDs(t, dm.body)
	if len(ids) != 1 || !strings.HasSuffix(ids[0], ":g1") {
		t.Fatalf("DM forget buttons = %v, want one naming g1", ids)
	}
	card("second", "g1") // memory changed before the press
	press := func(n int, customID string) string {
		t.Helper()
		i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i" + string(rune('0'+n)), AppID: "app", Token: "tok",
			Type: discordgo.InteractionMessageComponent, ChannelID: "1", User: &discordgo.User{ID: "u1", Username: "member"},
			Data: discordgo.MessageComponentInteractionData{CustomID: customID, ComponentType: discordgo.ButtonComponent}}}
		before := len(fake.calls)
		g.handleComponent(g.sess, i)
		g.inflight.Wait()
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, c := range fake.calls[before:] {
			if c.method == http.MethodPatch && strings.HasSuffix(c.path, "/messages/@original") {
				return c.body
			}
		}
		t.Fatalf("the press edited nothing: %+v", fake.calls[before:])
		return ""
	}
	refreshed := forgetButtonIDs(t, press(1, ids[0]))
	if len(refreshed) != 1 || refreshed[0] == ids[0] || !strings.HasSuffix(refreshed[0], ":g1") {
		t.Fatalf("refreshed forget buttons = %v (first %s), want a new one naming g1", refreshed, ids[0])
	}
	press(2, refreshed[0])
	left := map[string]bool{}
	rows, err := g.svc.Store.DB.Query(`SELECT id FROM person_card_item WHERE person_ref=?`, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left[id] = true
	}
	if left["first"] || left["second"] || !left["control"] {
		t.Fatalf("card lines left after forgetting g1: %v (g2's control must stay)", left)
	}
}
