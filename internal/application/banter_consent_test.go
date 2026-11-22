package application_test

import (
	"errors"
	"testing"

	"masamong/internal/application"
)

// Banter is an unasked line: a person who withdrew participation ("먼저 말 걸기") or memory consent
// gets neither a line nor an emoji when they talk about the bot. Someone who did not withdraw still does.
func TestBanterRespectsConsentWithdrawal(t *testing.T) {
	for _, scope := range []string{"participation", "memory"} {
		t.Run(scope, func(t *testing.T) {
			svc, _ := banterTestService(t)
			if out, err := runBanterTurn(svc, "before"); err != nil || out.Answer == "" {
				t.Fatalf("precondition: banter answers before the withdrawal (answer=%q err=%v)", out.Answer, err)
			}
			if r := asUser(svc, "speaker-id", "privacy.consent", map[string]any{"scope": scope, "decision": "withdraw"}); !r.OK {
				t.Fatalf("withdraw: %+v", r.Error)
			}
			out, err := runBanterTurn(svc, "after")
			var react application.ReactOnly
			if out.Answer != "" || errors.As(err, &react) || !errors.Is(err, application.ErrNotAddressed) {
				t.Fatalf("banter reached a person who withdrew %s consent: answer=%q err=%v", scope, out.Answer, err)
			}
		})
	}
}
