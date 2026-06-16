package mcp

// Phase 4 (#1534) tests: the run_automation meta-tool, @mcp-promoted automation
// dispatch + reflection, and @mcp-promoted query/mutation reflection + dispatch.
// A fake AutomationRunner records the dispatch; the fake engine carries the
// promoted-function surface.

import (
	"context"
	"strings"
	"testing"
)

type fakeRunner struct {
	owner, name string
	input       map[string]any
	dryRun      bool
	called      bool
	promoted    []string // names reported as @mcp automations
}

func (r *fakeRunner) RunAutomation(ctx context.Context, owner, name string, input map[string]any, dryRun bool) (map[string]any, error) {
	r.called, r.owner, r.name, r.input, r.dryRun = true, owner, name, input, dryRun
	return map[string]any{"status": "completed", "name": name}, nil
}
func (r *fakeRunner) PromotedAutomationTools() []map[string]any {
	out := make([]map[string]any, 0, len(r.promoted))
	for _, n := range r.promoted {
		out = append(out, map[string]any{"name": n, "description": n + " auto", "inputSchema": map[string]any{"type": "object"}})
	}
	return out
}
func (r *fakeRunner) IsPromotedAutomation(name string) bool {
	for _, n := range r.promoted {
		if n == name {
			return true
		}
	}
	return false
}

func withRunnerCtx(owner string, r AutomationRunner) context.Context {
	return withMCPAutomationRunner(withMCPSession(context.Background(), owner, newAuthoredRegistry()), r)
}

// run_automation routes name/input/dry_run to the runner under the session owner.
func TestRunAutomation_RoutesToRunner(t *testing.T) {
	eng := newFakeEngine()
	r := &fakeRunner{}
	ctx := withRunnerCtx("owner-1", r)
	res := callMCPTool(ctx, eng, "writer", TierAuthoring, toolRunAutomation,
		map[string]any{"name": "nightlySweep", "input": map[string]any{"limit": 10}, "dry_run": true})
	if isError(res) {
		t.Fatalf("run_automation should succeed, got %v", res)
	}
	if !r.called || r.name != "nightlySweep" || r.owner != "owner-1" || !r.dryRun {
		t.Errorf("runner got name=%q owner=%q dryRun=%v, want nightlySweep/owner-1/true", r.name, r.owner, r.dryRun)
	}
	if r.input["limit"] != 10 {
		t.Errorf("runner input = %v, want {limit:10}", r.input)
	}
}

// run_automation without a wired runner reports unavailable (not a crash).
func TestRunAutomation_Unavailable(t *testing.T) {
	eng := newFakeEngine()
	// session but no runner attached.
	ctx := withMCPSession(context.Background(), "owner-1", newAuthoredRegistry())
	res := callMCPTool(ctx, eng, "writer", TierAuthoring, toolRunAutomation, map[string]any{"name": "x"})
	if !isError(res) || !strings.Contains(resultText(res), "unavailable") {
		t.Fatalf("expected unavailable error, got %v", res)
	}
}

// run_automation requires a name.
func TestRunAutomation_RequiresName(t *testing.T) {
	eng := newFakeEngine()
	r := &fakeRunner{}
	res := callMCPTool(withRunnerCtx("owner-1", r), eng, "writer", TierAuthoring, toolRunAutomation, map[string]any{})
	if !isError(res) {
		t.Fatalf("missing name should error, got %v", res)
	}
	if r.called {
		t.Error("runner should not be called without a name")
	}
}

// A first-class @mcp automation is dispatched by its own name, with the call
// args (minus meta keys) as the input, and a non-dry run.
func TestPromotedAutomation_DispatchedByName(t *testing.T) {
	eng := newFakeEngine()
	r := &fakeRunner{promoted: []string{"sendDigest"}}
	res := callMCPTool(withRunnerCtx("owner-1", r), eng, "writer", TierAuthoring, "sendDigest",
		map[string]any{"audience": "all"})
	if isError(res) {
		t.Fatalf("promoted automation call should succeed, got %v", res)
	}
	if !r.called || r.name != "sendDigest" || r.dryRun {
		t.Errorf("runner got name=%q dryRun=%v, want sendDigest/false", r.name, r.dryRun)
	}
	if r.input["audience"] != "all" {
		t.Errorf("promoted automation input = %v, want {audience:all}", r.input)
	}
}

// @mcp-promoted query/mutation surface as first-class tools in tools/list.
func TestPromotedFunction_Listed(t *testing.T) {
	eng := newFakeEngine()
	eng.promotedFnTools = []map[string]any{{"name": "mySpaces", "description": "d", "inputSchema": map[string]any{"type": "object"}}}
	names := toolNames(listMCPTools(eng, "reader", TierSealed))
	if !names["mySpaces"] {
		t.Error("@mcp-promoted function should appear in tools/list (even in sealed tier -- it's a run-class op)")
	}
}

// Calling a promoted query by its own name routes to the named-construct
// executor (ExecuteAuthored when a session is present), not ExecuteToolByName.
func TestPromotedFunction_DispatchedByName(t *testing.T) {
	eng := newFakeEngine()
	eng.promotedFns = map[string]string{"mySpaces": "query"}
	ctx := withMCPSession(context.Background(), "owner-1", newAuthoredRegistry())
	res := callMCPTool(ctx, eng, "reader", TierAuthoring, "mySpaces", map[string]any{"limit": 3})
	if isError(res) {
		t.Fatalf("promoted query call should succeed, got %v", res)
	}
	if !strings.HasPrefix(eng.query, "mySpaces(") || !strings.Contains(eng.query, `"limit":3`) {
		t.Errorf("promoted query built %q, want mySpaces({\"limit\":3})", eng.query)
	}
	if eng.authoredOwner != "owner-1" {
		t.Errorf("promoted query should run under the session owner, got %q", eng.authoredOwner)
	}
	if eng.toolName == "mySpaces" {
		t.Error("a promoted function must NOT route through ExecuteToolByName")
	}
}
