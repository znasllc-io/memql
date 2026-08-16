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
// right combo PASSES the gate and authors the bundle into the session registry.
func TestGate_Define(t *testing.T) {
	eng := newFakeEngine()
	cases := []struct {
		name        string
		role        string
		tier        Tier
		wantRefused bool   // refused by a gate (vs. reached the authoring op)
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
			ctx := withMCPSession(context.Background(), "owner-1", newAuthoredRegistry())
			res := callMCPTool(ctx, eng, c.role, c.tier, toolDefine, map[string]any{"bundle": validSpecBundle})
			if c.wantRefused {
				if !isError(res) {
					t.Fatalf("expected gate refusal (isError), got %v", res)
				}
				txt := resultText(res)
				if !strings.Contains(txt, c.wantReason) {
					t.Errorf("refusal reason should mention %q, got %q", c.wantReason, txt)
				}
				// A gate refusal must NOT have authored anything.
				if strings.Contains(txt, `"defined"`) {
					t.Errorf("a refused define must author nothing, got %q", txt)
				}
				return
			}
			// Gate passed -> the bundle is authored.
			if isError(res) {
				t.Fatalf("gate-pass define should succeed, got error %v", res)
			}
			if !strings.Contains(resultText(res), "mcpSessionSpec") {
				t.Errorf("authored result should name the construct, got %q", resultText(res))
			}
		})
	}
}

// query (inline): gated by tier=inline AND role owner|developer.
func TestGate_InlineQuery(t *testing.T) {
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
			eng := newFakeEngine()
			res := callMCPTool(context.Background(), eng, c.role, c.tier, toolQuery, map[string]any{"query": "concept==v1:x:y"})
			if c.wantRefused {
				if !isError(res) {
					t.Fatalf("expected gate refusal (isError), got %v", res)
				}
				if !strings.Contains(resultText(res), c.wantReason) {
					t.Errorf("refusal reason should mention %q, got %q", c.wantReason, resultText(res))
				}
				if eng.inlineCalled {
					t.Error("a refused inline query must not reach the engine")
				}
				return
			}
			// Gate passed -> the inline query executes via ExecuteInline.
			if isError(res) {
				t.Fatalf("gate-pass inline query should execute, got error %v", res)
			}
			if !eng.inlineCalled || eng.query != "concept==v1:x:y" {
				t.Errorf("inline query should reach ExecuteInline with the text, got called=%v query=%q", eng.inlineCalled, eng.query)
			}
		})
	}
}

// tools/list only advertises define/stage/query when BOTH gates permit them, and
// promote strictly tighter than define (owner-only). define = owner|developer
// + authoring|inline; stage = the SAME bar as define (memql#3928); promote =
// owner only + authoring|inline.
//
// stage tracking define exactly is the assertion worth having: the tier exists
// because staging registers nothing shared and broadcasts nothing, so it is
// define made durable rather than a quieter promote. A stage listed at promote's
// bar would leave a developer with no durable option but the one that changes
// what everyone runs.
func TestGate_Listing(t *testing.T) {
	eng := newFakeEngine()
	cases := []struct {
		role                                      string
		tier                                      Tier
		wantDefine, wantStage, wantPromote, wantQ bool
	}{
		{"owner", TierSealed, false, false, false, false},
		{"owner", TierAuthoring, true, true, true, false},
		{"developer", TierAuthoring, true, true, false, false}, // developer authors + stages, cannot promote
		{"admin", TierAuthoring, false, false, false, false},   // admin gets none
		{"owner", TierInline, true, true, true, true},
		{"developer", TierInline, true, true, false, true},
		{"reader", TierInline, false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.role+"/"+c.tier.String(), func(t *testing.T) {
			names := toolNames(listMCPTools(eng, c.role, c.tier))
			if names[toolDefine] != c.wantDefine {
				t.Errorf("define listed=%v, want %v", names[toolDefine], c.wantDefine)
			}
			if names[toolStage] != c.wantStage {
				t.Errorf("stage listed=%v, want %v", names[toolStage], c.wantStage)
			}
			if names[toolStage] != names[toolDefine] {
				t.Errorf("stage listed=%v but define listed=%v -- they share a gate, so they must "+
					"list together or the tier is unreachable for whoever is missing it",
					names[toolStage], names[toolDefine])
			}
			if names[toolPromote] != c.wantPromote {
				t.Errorf("promote listed=%v, want %v", names[toolPromote], c.wantPromote)
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
