package discord

import (
	"testing"
	"testing/fstest"

	"masamong/internal/action"
)

// Two gateways in one process each speak from their own service's catalog: a locale bundle one operator
// loads never changes what the other gateway renders, whatever order they were made in.
func TestTwoGatewaysKeepTheirOwnCatalog(t *testing.T) {
	svcA, svcB := svcForTest(t), svcForTest(t)
	bundle := fstest.MapFS{"en.json": {Data: []byte(`{"discord.render.help.title": "Custom title"}`)}}
	if err := svcB.I18n.LoadFS(bundle); err != nil {
		t.Fatal(err)
	}
	gA, gB := &Gateway{svc: svcA}, &Gateway{svc: svcB, msgs: svcB.I18n}

	help := func(g *Gateway) string {
		r := g.renderCommand(action.Result{OK: true, Action: "help.version"}, Command{Name: "help", Action: "help.version"}, "en", false)
		return r.Embed.Title
	}
	if got := help(gB); got != "Custom title" {
		t.Errorf("the gateway with the bundle renders %q, want its own title", got)
	}
	if got := help(gA); got == "Custom title" || got == "" {
		t.Errorf("the other gateway renders %q: it read the first gateway's catalog", got)
	}
	if gA.catalog() == gB.catalog() {
		t.Error("two gateways share one catalog")
	}
}
