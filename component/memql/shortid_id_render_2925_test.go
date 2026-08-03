package memql

import (
	"context"
	"os"
	"strings"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageCompiler "github.com/znasllc-io/memql/component/language/compiler"
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
// shortId() is the documented normaliser for exactly that (#1859): no concept
// argument, canonical-or-bare in and bare out, and a fixed point for the ids
// this tree mints -- though NOT idempotent in general. memql#2981 measured the
// residual class and closed it at the argument boundary rather than by changing
// the primitive, which stays on the wire-egress path untouched; see
// shortid_fixpoint_2981_test.go. It passes memqllint
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
// The only in-tree producer of these mutations is examples/deploypack, whose
// argString trims and forwards a bare id minted by id.NewShortId. (Not
// component/deploycontrol -- it calls createDeployment and
// updateDeploymentStatus, which this does not touch.) shortId() is a fixed
// point on a bare id, so the derived id for that caller is byte-identical
// before and after.
//
// Inputs that DO change are canonical ids, whitespace-padded ids, and SOME
// v<digits>-shaped strings ("foo:v1:bar:baz:qux" strips to "qux";
// "a:v1:b:c" is unchanged) -- not canonical ids alone, which is what this
// comment claimed until review round 6 caught it standing after the same claim
// had been corrected in dsl/deployment/mutations.memql and the PR body. None
// is sent by an in-tree caller, and no rows exist to migrate (the mutation
// could not write at all until memql#2885).
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

// TestShortId_DoesNotMakeAColonBearingArgSafe records what shortId() does NOT
// do. memql#2980 closed the separator aliasing, but NOT here -- the two halves
// of that sentence are the point of this test, and #2980 rewrote its second
// half rather than deleting it.
//
// BareShortId strips only a RECOGNISABLE canonical id. Measured: "d:x" and
// "x:y:z" pass through unchanged. So shortId() closes the canonical-vs-bare
// duplication that authoring-rules.md §20 is about and contributes nothing to
// the separator aliasing: at the EVALUATOR level ("d:x","y") and ("d","x:y")
// still hash identically, and always will.
//
// What changed in #2980 is that ("d","x:y") can no longer BE evaluated. The
// aliasing is closed at the argument boundary by @pattern("^[^:]+$") on
// nodeType, not by normalisation, and the companion test below is where that
// is asserted. Keeping this one honest about the evaluator matters: a future
// reader must not conclude from #2980 that shortId() became colon-safe.
func TestShortId_DoesNotMakeAColonBearingArgSafe(t *testing.T) {
	for _, in := range []string{"d:x", "x:y:z", "deployment:abc123"} {
		if got := BareShortId(in); got != in {
			t.Errorf("BareShortId(%q) = %q -- if it now strips non-canonical colon forms, "+
				"shortId() may close the separator-aliasing hazard too, and "+
				"dsl/deployment/mutations.memql's comment about it needs updating.", in, got)
		}
	}
	// The aliasing at the evaluator level, still live BY DESIGN -- #2980 fixed
	// this by rejecting the input, not by changing the derivation, precisely so
	// that no already-derived id moves.
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
	if a, b := derive("d:x", "y"), derive("d", "x:y"); a != b {
		t.Errorf("the evaluator no longer aliases -- the id DERIVATION changed. #2980 was closed "+
			"by rejecting a colon-bearing nodeType at the argument boundary, specifically so that "+
			"every already-derived id stays byte-identical. If the derivation itself moved, every "+
			"existing v1:cluster:deploymentNodeSpec row needs re-keying and this is a migration.\n"+
			"  (\"d:x\",\"y\") -> %v\n  (\"d\",\"x:y\") -> %v", a, b)
	}
}

// TestNodeTypePatternClosesTheSeparatorAliasing is memql#2980's assertion.
//
// The hazard: id = hash(concat(shortId(deploymentId), ":", nodeType)), so
// ("d:x","y") and ("d","x:y") derive one id and two distinct (deployment,
// nodeType) pairs collapse onto a single timeline.
//
// The fix constrains the TRAILING part only. With nodeType colon-free the
// split at the last colon is unique, so equal concatenations force equal
// parts. deploymentId is deliberately left unconstrained -- the mutation
// accepts the canonical "v1:cluster:deployment:<short>" form on purpose
// (TestShortId_CanonicalAndBareDeriveTheSameId above pins that), and @pattern
// validates the RAW arg before shortId() runs, so a colon ban there would
// reject the very shape memql#2925 landed to support.
//
// Asserted through validateArgsField -- the function the mutation path
// actually reaches (executeMutationFunctionCall -> validateFunctionArgs) --
// rather than by re-deriving the hash, because the fix is a rejection and a
// hash comparison cannot observe it.
func TestNodeTypePatternClosesTheSeparatorAliasing(t *testing.T) {
	// The pattern as authored in dsl/deployment/mutations.memql. Built through
	// convertArgsField so the compilation path is the real one: a @pattern that
	// fails to compile is a load error, not a silently-absent check.
	nodeType, err := convertArgsField(&languageParser.ArgsField{
		Name: "nodeType", Type: "string", Pattern: "^[^:]+$",
	})
	if err != nil {
		t.Fatalf("the @pattern on nodeType does not compile: %v", err)
	}
	if nodeType.patternRegex == nil {
		t.Fatal("convertArgsField did not compile the nodeType @pattern, so validateArgsField " +
			"would skip it silently and memql#2980 would be closed in name only.")
	}

	// The witness pair from the issue. Exactly one of the two must now be
	// refused -- that is what breaks the collision.
	admitted := func(node string) bool {
		return validateArgsField(map[string]any{"nodeType": node}, nodeType, "") == nil
	}
	if !admitted("y") {
		t.Error(`("d:x", "y") must still be ADMITTED -- it is a legitimate pair, and rejecting it ` +
			`would change behaviour for callers who did nothing wrong.`)
	}
	if admitted("x:y") {
		t.Error(`("d", "x:y") must be REJECTED: with a colon-bearing nodeType it derives the same ` +
			`id as ("d:x", "y") and the two pairs collapse onto one timeline (memql#2980).`)
	}

	// Every node type the tree actually uses must survive the constraint. A
	// pattern that closes the hazard by rejecting real input is not a fix.
	for _, real := range []string{
		"bff", "identity", "cognition", "agent", "planner", "voice", "workbench", "mcp",
	} {
		if !admitted(real) {
			t.Errorf("node type %q is rejected by the nodeType @pattern -- the constraint must "+
				"forbid the separator and nothing else.", real)
		}
	}

	// And the constraint must be exactly "no colon", not a stricter shape that
	// would reject a node type nobody has invented yet.
	for _, plausible := range []string{"my-node", "node_2", "Node.v2", "a/b"} {
		if !admitted(plausible) {
			t.Errorf("node type %q is rejected, but it contains no %q. The rule is that the "+
				"trailing part of a hashed composite key must be free of the SEPARATOR -- "+
				"tightening it further is a different decision and not this one.", plausible, ":")
		}
	}
}

// The test above builds the ArgsField by hand, so it proves the MECHANISM
// works -- a compiled @pattern rejects a colon -- and not that the authored
// file carries one. The AST-level check in dsl/composite_hashed_id_test.go is
// the mirror image: it proves the annotation is written and never drives it
// through the validator. Either could pass while the other's half was broken:
// a parser that dropped @pattern on the inline `string! @pattern(...)` form
// would leave both green and the hazard open.
//
// This closes them into one assertion: read the REAL dsl/deployment file,
// parse it, convert its args schema, and drive a colon-bearing nodeType
// through the same validator the mutation path reaches.
func TestAuthoredNodeTypePatternRejectsAColonEndToEnd(t *testing.T) {
	src, err := os.ReadFile("../../dsl/deployment/mutations.memql")
	if err != nil {
		t.Fatalf("read the authored mutations file: %v", err)
	}
	parsed, err := languageCompiler.ParseFileSource(string(src))
	if err != nil {
		t.Fatalf("parse the authored mutations file: %v", err)
	}

	var checked int
	for _, def := range parsed.Definitions {
		fn, ok := def.(*languageAst.FunctionDef)
		if !ok {
			continue
		}
		if fn.Name != "createDeploymentNodeSpec" && fn.Name != "updateDeploymentNodeSpec" {
			continue
		}
		var authored *languageParser.ArgsField
		if fn.ArgsSchema != nil {
			for _, a := range fn.ArgsSchema.Fields {
				if a != nil && a.Name == "nodeType" {
					authored = a
				}
			}
		}
		if authored == nil {
			t.Fatalf("%s: no nodeType arg", fn.Name)
		}
		field, err := convertArgsField(authored)
		if err != nil {
			t.Fatalf("%s: the authored @pattern does not compile: %v", fn.Name, err)
		}
		if field.patternRegex == nil {
			t.Fatalf("%s: nodeType carries no COMPILED @pattern. The annotation is either "+
				"absent from the file or the parser dropped it on the inline form -- either way "+
				"the composite id is aliasable again (memql#2980).", fn.Name)
		}

		if err := validateArgsField(map[string]any{"nodeType": "bff"}, field, ""); err != nil {
			t.Errorf("%s: a plain nodeType must be accepted, got %v", fn.Name, err)
		}
		for _, bad := range []string{"x:y", ":y", "x:", "y\n:z"} {
			if err := validateArgsField(map[string]any{"nodeType": bad}, field, ""); err == nil {
				t.Errorf("%s: nodeType %q was accepted -- the trailing key part must be "+
					"separator-free or ('d:x','y') and ('d','x:y') derive one id", fn.Name, bad)
			}
		}
		checked++
	}

	if checked != 2 {
		t.Fatalf("expected to check both composite-id mutations, checked %d -- if one was "+
			"renamed this test silently stopped covering it", checked)
	}
}
