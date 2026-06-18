//go:build mcp

package app

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestPromotedAutomations_Memoized pins the #1599 fix: the @mcp-promoted
// automation set is enumerated once and reused, so a connector's per-session
// tools/list no longer re-parses the whole automation tree on every session.
func TestPromotedAutomations_Memoized(t *testing.T) {
	// Cache-hit path: when the set is already loaded, it is returned without
	// touching the loader (here nil) -- proving repeated calls don't re-load.
	cached := []*automations.Automation{{Name: "promoted1"}}
	r := &mcpAutomationRunner{promotedLoaded: true, promoted: cached}
	got := r.promotedAutomations()
	if len(got) != 1 || got[0].Name != "promoted1" {
		t.Fatalf("expected the cached promoted set, got %v", got)
	}

	// First enumeration over a real loader flips the loaded flag; subsequent
	// calls return the SAME backing slice (memoized, not rebuilt).
	r2 := &mcpAutomationRunner{
		loader: automations.NewLoader(automations.LoaderOptions{Registry: concept.DefaultRegistry()}),
	}
	a := r2.promotedAutomations()
	if !r2.promotedLoaded {
		t.Fatalf("expected promotedLoaded=true after the first enumeration")
	}
	b := r2.promotedAutomations()
	if reflect.ValueOf(a).Pointer() != reflect.ValueOf(b).Pointer() {
		t.Fatalf("repeated promotedAutomations() rebuilt the slice; expected the memoized one")
	}
}
