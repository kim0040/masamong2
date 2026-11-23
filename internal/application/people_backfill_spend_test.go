package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/store"
)

// Backfill spend has its own cap per server: it does not count toward the monthly budget that gates chat
// (Fable review R7). Synthetic spend only.
func TestPeopleBackfillSpendDoesNotStopChat(t *testing.T) {
	t.Parallel()
	svc, _, _ := nameCallSvc(t, `{"to":"bot","tone":"neutral"}`)
	applyDoc(t, svc, func(d *config.Document) { d.Limits.MonthlyBudgetUSD = "5" })
	in := application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "1", Message: "@마사몽 안녕", Speaker: "Chris", SpeakerID: "uc"}
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatalf("control: %v", err)
	}
	// a $3 backfill plus two re-run pilots, recorded as the backfill records it (store.AddBackfillSpend)
	if err := store.AddBackfillSpend(svc.Store.DB, time.Now().UTC().Format("2006-01-02"), 5.2, 3000); err != nil {
		t.Fatal(err)
	}
	in.MessageID = "2"
	if _, err := svc.ChatTurn(context.Background(), in); errors.Is(err, application.ErrChatBudget) {
		t.Errorf("chat is refused for the monthly budget because of people_backfill spend: %v", err)
	}
}
