//go:build !noweb

package web

import (
	"slices"
	"testing"

	"masamong/internal/catalog"
)

// Every catalog Action points at a page the console actually serves, so the GUI and CLI
// advertise the same surface. Deferred Actions still need a real landing page (they render
// as unavailable there), never a dead link.
func TestCatalogGUIRoutesAreServed(t *testing.T) {
	served := PageRoutes()
	for _, d := range catalog.All() {
		if d.GUIRoute == "" {
			t.Fatalf("%s has no GUI route", d.ID)
		}
		if !slices.Contains(served, d.GUIRoute) {
			t.Fatalf("%s GUI route %s is not served by the console (served: %v)", d.ID, d.GUIRoute, served)
		}
	}
}

// The SSE stream lives at GET /api/events; no page may claim /events or /api paths.
func TestEventsPathIsNotAPage(t *testing.T) {
	if slices.Contains(PageRoutes(), "/events") {
		t.Fatal("/events is the SSE endpoint, not a page")
	}
}
