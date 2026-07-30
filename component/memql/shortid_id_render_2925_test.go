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
