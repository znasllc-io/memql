package mcp

// Phase 3 (#1533) Tier-2 authoring tests: the `define` op (validate + register
// session-scoped, owner-scoped, non-durable), session-aware call-by-name
// routing, and the owner-gated `promote` op. They mirror the gate-bypass
// structure of the Phase 2 tests and use the fake engine + a real authored
// registry (no live DB -- validation + registration are pure).

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// validSpecBundle is a context-spec: it compiles + binds with no concept
// dependency, so it validates cleanly through the Gate-1 sandbox.
const validSpecBundle = `@description("MCP session test spec")
spec mcpSessionSpec {
  actor.role == "admin"
}`

func newAuthoredRegistry() *memql.AuthoredRuntimeRegistry {
	return memql.NewAuthoredRuntimeRegistry()
}

// define authors into the caller's session registry, owner-scoped + callable by
// name afterward; it does NOT leak to another owner (the shared-schema isolation
// half of the acceptance).
func TestDefine_RegistersSessionScopedAndOwnerIsolated(t *testing.T) {
	eng := newFakeEngine()
	reg := newAuthoredRegistry()
	ctx := withMCPSession(context.Background(), "owner-1", reg)

	res := callMCPTool(ctx, eng, "developer", TierAuthoring, toolDefine, map[string]any{"bundle": validSpecBundle})
	if isError(res) {
		t.Fatalf("define should succeed, got %v", res)
	}
	if !strings.Contains(resultText(res), "mcpSessionSpec") {
		t.Fatalf("result should name the authored construct, got %q", resultText(res))
	}
	// Registered for owner-1...
	if _, ok := reg.Lookup("owner-1", "spec", "mcpSessionSpec"); !ok {
		t.Error("construct not registered for the authoring owner")
	}
	// ...and invisible to a different owner (owner-scoped, not shared).
	if _, ok := reg.Lookup("owner-2", "spec", "mcpSessionSpec"); ok {
		t.Error("a different owner can see the session-authored construct (isolation broken)")
	}
}

// define needs an authoring identity; without one it reports unavailable rather
// than authoring anonymously.
func TestDefine_UnavailableWithoutSessionIdentity(t *testing.T) {
	eng := newFakeEngine()
	// withMCPSession with an empty owner -> session not available.
	ctx := withMCPSession(context.Background(), "", newAuthoredRegistry())
	res := callMCPTool(ctx, eng, "owner", TierAuthoring, toolDefine, map[string]any{"bundle": validSpecBundle})
	if !isError(res) || !strings.Contains(resultText(res), "unavailable") {
		t.Fatalf("expected an unavailable error, got %v", res)
	}
}

// a bundle that fails validation registers nothing and surfaces diagnostics.
func TestDefine_RejectsInvalidBundle(t *testing.T) {
	eng := newFakeEngine()
	reg := newAuthoredRegistry()
	ctx := withMCPSession(context.Background(), "owner-1", reg)
	// Dangling operator -> Gate-1 compile failure.
	res := callMCPTool(ctx, eng, "developer", TierAuthoring, toolDefine,
		map[string]any{"bundle": `spec brokenSpec { actor.role == }`})
	if !isError(res) {
		t.Fatalf("invalid bundle should be an error result, got %v", res)
	}
	if reg.Count() != 0 {
		t.Errorf("a failed validation must register nothing, registry has %d", reg.Count())
	}
}

// run_query resolves against the session (ExecuteAuthored) when a session is
// present, and falls back to plain Execute when it is not -- the call-by-name
// wiring for session-authored constructs.
func TestRunQuery_RoutesThroughSessionWhenPresent(t *testing.T) {
	t.Run("with session -> ExecuteAuthored", func(t *testing.T) {
		eng := newFakeEngine()
		reg := newAuthoredRegistry()
		ctx := withMCPSession(context.Background(), "owner-1", reg)
		callMCPTool(ctx, eng, "reader", TierAuthoring, toolRunQuery, map[string]any{"name": "someAuthored"})
		if eng.authoredOwner != "owner-1" || eng.authoredReg != reg {
			t.Errorf("run_query with a session should route to ExecuteAuthored(owner=%q reg=%p), got owner=%q reg=%p",
				"owner-1", reg, eng.authoredOwner, eng.authoredReg)
		}
		if !strings.HasPrefix(eng.query, "someAuthored(") {
			t.Errorf("query = %q, want someAuthored(...)", eng.query)
		}
	})
	t.Run("no session -> Execute", func(t *testing.T) {
		eng := newFakeEngine()
		callMCPTool(context.Background(), eng, "reader", TierAuthoring, toolRunQuery, map[string]any{"name": "coreQuery"})
		if eng.authoredOwner != "" {
			t.Errorf("run_query without a session must use Execute, not ExecuteAuthored (owner=%q)", eng.authoredOwner)
		}
	})
}

// promote is OWNER-only: a developer (who may author) is refused, an owner is
// allowed through the gate.
func TestPromote_OwnerOnlyGate(t *testing.T) {
	cases := []struct {
		role        string
		tier        Tier
		wantRefused bool
		wantReason  string
	}{
		{"developer", TierAuthoring, true, "owner role"},
		{"writer", TierAuthoring, true, "owner role"},
		{"owner", TierSealed, true, "tier"},
		{"owner", TierAuthoring, false, ""},
	}
	for _, c := range cases {
		t.Run(c.role+"/"+c.tier.String(), func(t *testing.T) {
			eng := newFakeEngine()
			reg := newAuthoredRegistry()
			ctx := withMCPSession(context.Background(), "owner-1", reg)
			// Pre-author a construct so an allowed promote has something to find.
			if _, err := memql.AuthorSessionBundle(reg, "owner-1", validSpecBundle); err != nil {
				t.Fatalf("seed author: %v", err)
			}
			res := callMCPTool(ctx, eng, c.role, c.tier, toolPromote, map[string]any{"name": "mcpSessionSpec"})
			if c.wantRefused {
				if !isError(res) || !strings.Contains(resultText(res), c.wantReason) {
					t.Fatalf("expected refusal mentioning %q, got %v", c.wantReason, res)
				}
				if eng.promoted != nil {
					t.Error("a refused promote must not reach the engine")
				}
				return
			}
			if isError(res) {
				t.Fatalf("owner promote should succeed, got %v", res)
			}
			if eng.promoted == nil || eng.promoted.Name != "mcpSessionSpec" {
				t.Errorf("promote should pass the construct to the engine, got %+v", eng.promoted)
			}
		})
	}
}

// promoting a name the session never defined is a not-found error, not a silent
// pass to the engine.
func TestPromote_UnknownConstruct(t *testing.T) {
	eng := newFakeEngine()
	reg := newAuthoredRegistry()
	ctx := withMCPSession(context.Background(), "owner-1", reg)
	res := callMCPTool(ctx, eng, "owner", TierAuthoring, toolPromote, map[string]any{"name": "neverDefined"})
	if !isError(res) || !strings.Contains(resultText(res), "no session-authored construct") {
		t.Fatalf("expected a not-found error, got %v", res)
	}
	if eng.promoted != nil {
		t.Error("unknown construct must not reach the engine")
	}
}
