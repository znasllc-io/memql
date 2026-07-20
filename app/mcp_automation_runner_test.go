//go:build mcp

package app

import (
	"reflect"
	"strings"
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

// TestAdvertisableAutomations_DisabledDropped pins memql#2681: a
// @disabled @mcp automation must not appear on the advertised MCP tool
// surface -- the dishonest-surface class #2647/#2682 closed for the
// function half.
func TestAdvertisableAutomations_DisabledDropped(t *testing.T) {
	disabled := false
	enabled := true
	got := advertisableAutomations([]*automations.Automation{
		{Name: "disabledPromoted", MCPPromoted: true, Enabled: &disabled},
		{Name: "enabledPromoted", MCPPromoted: true, Enabled: &enabled},
		// nil Enabled means enabled (IsEnabled's documented default), so an
		// unannotated promoted automation must still be advertised.
		{Name: "defaultPromoted", MCPPromoted: true},
		{Name: "notPromoted", MCPPromoted: false},
		nil,
	})
	names := make([]string, 0, len(got))
	for _, a := range got {
		names = append(names, a.Name)
	}
	if len(names) != 2 || names[0] != "defaultPromoted" || names[1] != "enabledPromoted" {
		t.Fatalf("advertised = %v; want [defaultPromoted enabledPromoted] (sorted; disabled and un-promoted dropped)", names)
	}
}

// TestAutomationRunRefusal pins the #2605 analogue for automations
// (memql#2681): dropping a @disabled automation from the surface alone
// would leave it RUNNABLE by name. The refusal message must name the
// automation and the cause.
func TestAutomationRunRefusal(t *testing.T) {
	disabled := false
	err := automationRunRefusal(&automations.Automation{Name: "disabledAuto", Enabled: &disabled})
	if err == nil {
		t.Fatal("a @disabled automation must be refused")
	}
	if !strings.Contains(err.Error(), "@disabled") || !strings.Contains(err.Error(), "disabledAuto") {
		t.Errorf("message must name the automation and the cause: %v", err)
	}
	enabled := true
	if err := automationRunRefusal(&automations.Automation{Name: "ok", Enabled: &enabled}); err != nil {
		t.Errorf("an enabled automation must run: %v", err)
	}
	// nil Enabled means enabled.
	if err := automationRunRefusal(&automations.Automation{Name: "dflt"}); err != nil {
		t.Errorf("an unannotated automation must run: %v", err)
	}
}
