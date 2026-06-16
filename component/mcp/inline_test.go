package mcp

// Phase 5 (#1535) Tier-3 inline `query` tool tests: both gates (inline tier +
// CanRunInline) guard it, the query text reaches ExecuteInline under the session
// owner, and tools/list advertises it only when both gates permit.

import (
	"context"
	"strings"
	"testing"
)

// query executes inline text via ExecuteInline under the session owner when the
// inline tier + owner/developer gates pass.
func TestInlineQuery_ExecutesUnderSession(t *testing.T) {
	eng := newFakeEngine()
	ctx := withMCPSession(context.Background(), "owner-1", newAuthoredRegistry())
	res := callMCPTool(ctx, eng, "developer", TierInline, toolQuery,
		map[string]any{"query": "concept==v1:cognition:space"})
	if isError(res) {
		t.Fatalf("inline query should execute, got %v", res)
	}
	if !eng.inlineCalled {
		t.Error("inline query must route to ExecuteInline (not Execute/ExecuteToolByName)")
	}
	if eng.query != "concept==v1:cognition:space" {
		t.Errorf("inline query text = %q", eng.query)
	}
	if eng.authoredOwner != "owner-1" {
		t.Errorf("inline query should run under the session owner, got %q", eng.authoredOwner)
	}
}

// Gate-bypass: a wrong tier OR a wrong role is refused before the engine.
func TestInlineQuery_GateBypass(t *testing.T) {
	cases := []struct {
		role       string
		tier       Tier
		wantReason string
	}{
		{"developer", TierAuthoring, "tier"}, // needs inline tier
		{"owner", TierSealed, "tier"},
		{"admin", TierInline, "role"},  // admin cannot run inline
		{"writer", TierInline, "role"}, // writer cannot run inline
		{"reader", TierInline, "role"},
	}
	for _, c := range cases {
		t.Run(c.role+"/"+c.tier.String(), func(t *testing.T) {
			eng := newFakeEngine()
			ctx := withMCPSession(context.Background(), "owner-1", newAuthoredRegistry())
			res := callMCPTool(ctx, eng, c.role, c.tier, toolQuery, map[string]any{"query": "concept==v1:x:y"})
			if !isError(res) || !strings.Contains(resultText(res), c.wantReason) {
				t.Fatalf("expected refusal mentioning %q, got %v", c.wantReason, res)
			}
			if eng.inlineCalled {
				t.Error("a refused inline query must not reach the engine")
			}
		})
	}
}

// query requires inline 'query' text.
func TestInlineQuery_RequiresText(t *testing.T) {
	eng := newFakeEngine()
	ctx := withMCPSession(context.Background(), "owner-1", newAuthoredRegistry())
	res := callMCPTool(ctx, eng, "owner", TierInline, toolQuery, map[string]any{})
	if !isError(res) {
		t.Fatalf("missing query text should error, got %v", res)
	}
	if eng.inlineCalled {
		t.Error("engine must not be called without query text")
	}
}

// tools/list advertises `query` only in the inline tier for owner/developer.
func TestInlineQuery_Listing(t *testing.T) {
	eng := newFakeEngine()
	cases := []struct {
		role string
		tier Tier
		want bool
	}{
		{"owner", TierInline, true},
		{"developer", TierInline, true},
		{"admin", TierInline, false},
		{"owner", TierAuthoring, false}, // wrong tier
		{"reader", TierInline, false},
	}
	for _, c := range cases {
		t.Run(c.role+"/"+c.tier.String(), func(t *testing.T) {
			names := toolNames(listMCPTools(eng, c.role, c.tier))
			if names[toolQuery] != c.want {
				t.Errorf("query listed=%v, want %v", names[toolQuery], c.want)
			}
		})
	}
}
