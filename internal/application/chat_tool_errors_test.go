package application

import (
	"testing"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

func TestChatToolViewKeepsUnconfiguredAsSetupState(t *testing.T) {
	for _, res := range []action.Result{
		action.OK(action.Request{Action: "search.web"}, map[string]any{"status": "unconfigured", "message": "operator detail"}),
		action.Fail(action.Request{Action: "finance.quote"}, errcode.ProviderUnconfigured, "provider.unconfigured", "operator detail", nil),
	} {
		got := chatView(res)
		if got.OK || got.Status != "unconfigured" || got.Error != "" {
			t.Fatalf("tool setup state was hidden as transient failure: %+v", got)
		}
		if message, _ := got.Data["message"].(string); message != "" {
			t.Fatalf("operator-only setup detail leaked to the member: %+v", got.Data)
		}
	}
}
