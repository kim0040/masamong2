package discord

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// The slash command's amount is a Discord number option; it reaches the card as the amount typed.
func TestFXSlashAmountOption(t *testing.T) {
	d := newFXDelivery(t, svcForTest(t), nil)
	d.slash(t, strOption("currency", "엔"),
		&discordgo.ApplicationCommandInteractionDataOption{Name: "amount", Type: discordgo.ApplicationCommandOptionNumber, Value: float64(50000)})
	if flags := d.deferredFlags(t); flags != 0 {
		t.Errorf("flags = %d, want a public answer", flags)
	}
	edit, ok := d.fake.find(http.MethodPatch, "/messages/@original")
	if !ok {
		t.Fatalf("no card: %+v", d.fake.calls)
	}
	if !strings.Contains(edit.body, "50,000엔 ≈ 50,000원") {
		t.Errorf("card = %s", edit.body)
	}
	if got := d.calls.Load(); got != 1 || d.paths[0] != "/latest/JPY" {
		t.Errorf("provider calls %d %v, want one for the currency asked about", got, d.paths)
	}
}

// A very large number option must not turn into scientific notation on the way in.
func TestFXSlashLargeAmountOption(t *testing.T) {
	d := newFXDelivery(t, svcForTest(t), nil)
	d.slash(t, &discordgo.ApplicationCommandInteractionDataOption{Name: "amount", Type: discordgo.ApplicationCommandOptionNumber, Value: float64(1000000)})
	edit, ok := d.fake.find(http.MethodPatch, "/messages/@original")
	if !ok || !strings.Contains(edit.body, "1,000,000원 ≈ ") {
		t.Fatalf("card = %+v", edit)
	}
}
