package mcp

// Phase 2 (#1532) capability-tier gate tests. They assert "Gate A" (the
// deployment tier) and "Gate B" (the acting role) BOTH guard the authoring
// (`define`) and inline (`query`) ops: a wrong tier OR a wrong role can never
// reach a gated op. Uses the fake engine from tool_surface_test.go.

import (
	"context"
	"strings"
	"testing"
)

func TestParseTier(t *testing.T) {
	cases := map[string]Tier{
		"sealed": TierSealed, "SEALED": TierSealed,
		"authoring": TierAuthoring, "": TierAuthoring, "nonsense": TierAuthoring,
		"inline": TierInline, " Inline ": TierInline,
	}
	for in, want := range cases {
		if got := ParseTier(in); got != want {
			t.Errorf("ParseTier(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTierAllows(t *testing.T) {
	cases := []struct {
		tier  Tier
		class opClass
		want  bool
	}{
		{TierSealed, classExec, true}, {TierSealed, classAuthor, false}, {TierSealed, classInline, false},
		{TierAuthoring, classExec, true}, {TierAuthoring, classAuthor, true}, {TierAuthoring, classInline, false},
		{TierInline, classExec, true}, {TierInline, classAuthor, true}, {TierInline, classInline, true},
	}
	for _, c := range cases {
		if got := tierAllows(c.tier, c.class); got != c.want {
			t.Errorf("tierAllows(%v, %v) = %v, want %v", c.tier, c.class, got, c.want)
		}
	}
}

func isError(res map[string]any) bool { b, _ := res["isError"].(bool); return b }
func resultText(res map[string]any) string {
	content, _ := res["content"].([]map[string]any)
	if len(content) > 0 {
		return content[0]["text"].(string)
	}
	return ""
}

// define: gated by tier=authoring|inline (Gate A) AND role owner|developer
// (Gate B). A wrong tier OR wrong role is REFUSED (never reaches the op); the
// right combo PASSES the gate to the "not yet implemented" stub.
func TestGate_Define(t *testing.T) {
	eng := newFakeEngine()
	cases := []struct {
		name        string
		role        string
		tier        Tier
		wantRefused bool   // refused by a gate (vs. reached the not-yet stub)
		wantReason  string // substring of the refusal message
	}{
		{"sealed tier blocks owner", "owner", TierSealed, true, "tier"},
		{"authoring tier, admin role blocked", "admin", TierAuthoring, true, "role"},
		{"authoring tier, writer role blocked", "writer", TierAuthoring, true, "role"},
		{"authoring tier, owner passes gate", "owner", TierAuthoring, false, ""},
		{"authoring tier, developer passes gate", "developer", TierAuthoring, false, ""},
		{"inline tier, developer passes gate", "developer", TierInline, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callMCPTool(context.Background(), eng, c.role, c.tier, toolDefine, map[string]any{"bundle": "x"})
			if !isError(res) {
				t.Fatalf("define should always be isError in Phase 2, got %v", res)
			}
			txt := resultText(res)
			refused := !strings.Contains(txt, "not yet implemented")
			if refused != c.wantRefused {
				t.Errorf("refused=%v, want %v (msg=%q)", refused, c.wantRefused, txt)
			}
			if c.wantRefused && !strings.Contains(txt, c.wantReason) {
				t.Errorf("refusal reason should mention %q, got %q", c.wantReason, txt)
			}
		})
	}
}

// query (inline): gated by tier=inline AND role owner|developer.
func TestGate_InlineQuery(t *testing.T) {
	eng := newFakeEngine()
	cases := []struct {
		name        string
		role        string
		tier        Tier
		wantRefused bool
		wantReason  string
	}{
		{"authoring tier blocks owner (needs inline)", "owner", TierAuthoring, true, "tier"},
		{"sealed tier blocks developer", "developer", TierSealed, true, "tier"},
		{"inline tier, admin role blocked", "admin", TierInline, true, "role"},
		{"inline tier, owner passes gate", "owner", TierInline, false, ""},
		{"inline tier, developer passes gate", "developer", TierInline, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := callMCPTool(context.Background(), eng, c.role, c.tier, toolQuery, map[string]any{"query": "x"})
			if !isError(res) {
				t.Fatalf("inline query should be isError in Phase 2, got %v", res)
			}
			txt := resultText(res)
			refused := !strings.Contains(txt, "not yet implemented")
			if refused != c.wantRefused {
				t.Errorf("refused=%v, want %v (msg=%q)", refused, c.wantRefused, txt)
			}
			if c.wantRefused && !strings.Contains(txt, c.wantReason) {
				t.Errorf("refusal reason should mention %q, got %q", c.wantReason, txt)
			}
		})
	}
}

// tools/list only advertises define/query when BOTH gates permit them.
func TestGate_Listing(t *testing.T) {
	eng := newFakeEngine()
	cases := []struct {
		role              string
		tier              Tier
		wantDefine, wantQ bool
	}{
		{"owner", TierSealed, false, false},
		{"owner", TierAuthoring, true, false},
		{"developer", TierAuthoring, true, false},
		{"admin", TierAuthoring, false, false}, // admin gets neither
		{"owner", TierInline, true, true},
		{"developer", TierInline, true, true},
		{"reader", TierInline, false, false},
	}
	for _, c := range cases {
		t.Run(c.role+"/"+c.tier.String(), func(t *testing.T) {
			names := toolNames(listMCPTools(eng, c.role, c.tier))
			if names[toolDefine] != c.wantDefine {
				t.Errorf("define listed=%v, want %v", names[toolDefine], c.wantDefine)
			}
			if names[toolQuery] != c.wantQ {
				t.Errorf("query listed=%v, want %v", names[toolQuery], c.wantQ)
			}
			// run_* meta-tools are tier-1 and always listed.
			for _, mt := range []string{toolRunQuery, toolRunMutation, toolRunAutomation} {
				if !names[mt] {
					t.Errorf("tier-1 meta-tool %q must always be listed", mt)
				}
			}
		})
	}
}
