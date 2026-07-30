package memql

import (
	"context"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// memql#2925 blocker 2: shortId() parses in an `id:` slot and dies at render.
//
// `authoring-rules.md` §20 requires a hashed FK arg to be NORMALISED before it
// is hashed, because the hash is byte-level -- two callers passing the same
// logical reference under different shapes ("user-abc" vs
// "_system:v1:identity:user:user-abc") hash to different strings and produce
// DUPLICATE rows with distinct ids.
//
// shortId() is the documented normaliser for exactly that (#1859): idempotent,
// no concept argument, canonical-or-bare in and bare out. It passes memqllint
// in the `id:` position, then fails at render with
//
//	evaluate id: unsupported expression in mutation template: *ast.ShortIdExpr
//
// because evalParserExpression has cases for ConcatExpr and HashExpr and none
// for ShortIdExpr. So the builtin works in payload positions
// (dsl/forge/mutations.memql `requestId: shortId(args.requestId)`) and not in
// the id derivation -- the position §20 is about.
//
// That lint-clean-then-render-fail split is the memql#2909 class, and worth
// closing on its own regardless of which normaliser a given mutation picks.
func TestShortId_RendersInAnIdDerivation(t *testing.T) {
	eval := &mutationTemplateEvaluator{args: map[string]any{
		"deploymentId": "v1:cluster:deployment:abc123",
	}}
	expr := &languageParser.ShortIdExpr{
		Target: &languageParser.ArgRefExpr{Path: "deploymentId"},
	}
	got, err := eval.evalParserExpression(context.Background(), expr)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported expression in mutation template") {
			t.Fatalf("memql#2925 verbatim: shortId() lints clean in an `id:` slot and cannot "+
				"render, so the normaliser authoring-rules.md §20 requires is unavailable in "+
				"the one position §20 is about.\n  error: %v", err)
		}
		t.Fatalf("shortId() in an id derivation: %v", err)
	}
	if got != "abc123" {
		t.Errorf("shortId(canonical) must yield the bare short id; got %#v", got)
	}
}

// The property §20 actually wants: canonical and bare inputs must derive the
// SAME id. This is the test the issue asks to restore -- it was written during
// memql#2885 and removed when the normalisation could not land.
func TestShortId_CanonicalAndBareDeriveTheSameId(t *testing.T) {
	derive := func(t *testing.T, deploymentID string) any {
		t.Helper()
		eval := &mutationTemplateEvaluator{args: map[string]any{
			"deploymentId": deploymentID,
			"nodeType":     "bff",
		}}
		// hash(concat(shortId(args.deploymentId), ":", args.nodeType)) --
		// §20's prescribed shape: normalise the FK, then hash the composite.
		expr := &languageParser.HashExpr{
			Target: &languageParser.ConcatExpr{Args: []languageParser.ExpressionNode{
				&languageParser.ShortIdExpr{
					Target: &languageParser.ArgRefExpr{Path: "deploymentId"},
				},
				&languageParser.LiteralExpr{Value: ":"},
				&languageParser.ArgRefExpr{Path: "nodeType"},
			}},
		}
		got, err := eval.evalParserExpression(context.Background(), expr)
		if err != nil {
			t.Fatalf("deriving from %q: %v", deploymentID, err)
		}
		return got
	}

	bare := derive(t, "abc123")
	canonical := derive(t, "v1:cluster:deployment:abc123")
	if bare != canonical {
		t.Errorf("§20's invariant: the same logical deployment under two shapes must derive ONE "+
			"id, or the same (deployment, nodeType) gets two timelines and "+
			"nodeSpecsForDeployment returns both.\n  bare      -> %v\n  canonical -> %v",
			bare, canonical)
	}
}

// TestShortId_IsANoOpForEveryInTreeProducer is the safety argument for adding
// shortId() to dsl/deployment/mutations.memql, asserted rather than assumed.
//
// Every in-tree producer passes a BARE deploymentId --
// component/deploycontrol/deployment_store.go mints one with id.NewShortId(),
// and examples/deploypack forwards the bare payload field. shortId() is
// idempotent on a bare value, so the derived id for those callers is
// byte-identical before and after. The behaviour only changes for a canonical
// input, which no in-tree producer sends and for which no rows exist (the
// mutation could not write at all until memql#2885).
func TestShortId_IsANoOpForEveryInTreeProducer(t *testing.T) {
	derive := func(t *testing.T, normalise bool) any {
		t.Helper()
		eval := &mutationTemplateEvaluator{args: map[string]any{
			"deploymentId": "9f8e7d6c-1234-4abc-9def-000000000001", // id.NewShortId shape
			"nodeType":     "bff",
		}}
		var fk languageParser.ExpressionNode = &languageParser.ArgRefExpr{Path: "deploymentId"}
		if normalise {
			fk = &languageParser.ShortIdExpr{Target: fk}
		}
		got, err := eval.evalParserExpression(context.Background(), &languageParser.HashExpr{
			Target: &languageParser.ConcatExpr{Args: []languageParser.ExpressionNode{
				fk,
				&languageParser.LiteralExpr{Value: ":"},
				&languageParser.ArgRefExpr{Path: "nodeType"},
			}},
		})
		if err != nil {
			t.Fatalf("deriving (normalise=%v): %v", normalise, err)
		}
		return got
	}
	if before, after := derive(t, false), derive(t, true); before != after {
		t.Errorf("adding shortId() must not change the id any in-tree producer derives -- there "+
			"is no migration in this PR because there is nothing to migrate.\n"+
			"  without shortId -> %v\n  with shortId    -> %v", before, after)
	}
}

// TestShortId_DoesNotMakeAColonBearingArgSafe records what this fix does NOT
// do, because the mutation's comment block claims a separate hazard and I am
// not going to let a fix quietly appear to cover it.
//
// BareShortId strips only a RECOGNISABLE canonical id. Measured: "d:x" and
// "x:y:z" pass through unchanged. So shortId() closes the
// canonical-vs-bare duplication that authoring-rules.md §20 is about, and does
// nothing for the separator aliasing -- ("d:x","y") and ("d","x:y") still hash
// identically. That remains safe only because id.NewShortId mints a colon-free
// uuid, and remains reachable through the generated SDK.
func TestShortId_DoesNotMakeAColonBearingArgSafe(t *testing.T) {
	for _, in := range []string{"d:x", "x:y:z", "deployment:abc123"} {
		if got := BareShortId(in); got != in {
			t.Errorf("BareShortId(%q) = %q -- if it now strips non-canonical colon forms, "+
				"shortId() may close the separator-aliasing hazard too, and "+
				"dsl/deployment/mutations.memql's comment about it needs updating.", in, got)
		}
	}
	// The aliasing itself, still live.
	derive := func(dep, node string) any {
		eval := &mutationTemplateEvaluator{args: map[string]any{"deploymentId": dep, "nodeType": node}}
		got, err := eval.evalParserExpression(context.Background(), &languageParser.HashExpr{
			Target: &languageParser.ConcatExpr{Args: []languageParser.ExpressionNode{
				&languageParser.ShortIdExpr{Target: &languageParser.ArgRefExpr{Path: "deploymentId"}},
				&languageParser.LiteralExpr{Value: ":"},
				&languageParser.ArgRefExpr{Path: "nodeType"},
			}},
		})
		if err != nil {
			t.Fatalf("deriving (%q,%q): %v", dep, node, err)
		}
		return got
	}
	// Asserted through the EVALUATOR, not the primitive, so a change that
	// strips colons inside the ShortIdExpr case is caught too -- that mutation
	// escaped the primitive-only assertions above.
	if a, b := derive("d:x", "y"), derive("d", "x:y"); a != b {
		t.Errorf("the separator aliasing appears CLOSED: (\"d:x\",\"y\") and (\"d\",\"x:y\") now "+
			"derive different ids. That is an improvement, but the comment block in "+
			"dsl/deployment/mutations.memql still documents the aliasing as live and memql#2925 "+
			"still tracks it -- update both in the same change.\n  (\"d:x\",\"y\") -> %v\n"+
			"  (\"d\",\"x:y\") -> %v", a, b)
	}
}
