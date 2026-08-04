package memql

import (
	"context"
	"testing"
)

// hash_fixed_width_test.go -- memql#3009 landing review.
//
// The per-part id derivation this issue introduced,
// hash(concat(hash(a), hash(b))), is injective for exactly one reason: every
// part renders to a fixed 64 characters, so the concatenation has exactly one
// decomposition. That is the whole argument, and it is only true while hash()
// is fixed-width for EVERY input.
//
// It was not. RuntimeEvaluator.EvaluateHash returned "" for a nil input, so an
// absent part contributed zero width and (absent, X) concatenated to the same
// string as (X, absent) -- the aliasing this issue exists to close, one layer
// down and stated in-tree as if it were structural.
//
// The reachable site is dsl/cognition/logic.memql, which derives an id from two
// event-payload fields that can both be absent. Logic evaluates through the
// runtime evaluator; mutation `insert` templates evaluate through
// mutationTemplateEvaluator, which already normalised missing to "". So the
// same authored hash(x) had two widths depending on which construct held it,
// and only one of the two was safe.

// Both evaluators, one rule. A width that depends on the construct an
// expression sits in is a property nobody can reason about at the call site.
func TestHashIsFixedWidthOnBothEvaluators(t *testing.T) {
	const sha256HexLen = 64

	t.Run("runtime evaluator", func(t *testing.T) {
		re := &RuntimeEvaluator{}
		for _, tc := range []struct {
			name string
			in   any
		}{
			{"nil", nil},
			{"empty string", ""},
			{"a string", "chat"},
			{"a number", 42},
			{"a bool", false},
		} {
			if got := re.EvaluateHash(tc.in); len(got) != sha256HexLen {
				t.Errorf("EvaluateHash(%s) is %d chars, want %d.\n\n"+
					"A composite id is hash(concat(hash(a), hash(b))), which is injective ONLY "+
					"because every part is the same width. A short part means (absent, X) and "+
					"(X, absent) concatenate identically and derive one id -- the memql#3009 "+
					"aliasing, reintroduced below the fix.", tc.name, len(got), sha256HexLen)
			}
		}
	})

	t.Run("mutation template evaluator", func(t *testing.T) {
		eval := &mutationTemplateEvaluator{args: map[string]any{
			"present": "x",
			"action":  map[string]any{"type": "chat"},
		}}
		for _, expr := range []string{
			`hash(args.present)`,
			`hash(args.absent)`,
			`hash(args.action.type)`,
			`hash(args.action.idempotencyKey)`, // nested absent
			`hash("")`,
		} {
			got, err := eval.evalHash(context.Background(), expr)
			if err != nil {
				t.Fatalf("%s: %v", expr, err)
			}
			if len(got) != sha256HexLen {
				t.Errorf("%s is %d chars, want %d -- see the runtime-evaluator case above for why",
					expr, len(got), sha256HexLen)
			}
		}
	})
}

// The property the width guarantees, asserted directly rather than inferred:
// an absent part must not let two different tuples derive one id.
func TestAbsentPartCannotAliasACompositeId(t *testing.T) {
	re := &RuntimeEvaluator{}

	// (absent, "x") vs ("x", absent) -- the minimal collision.
	first := re.EvaluateHash(re.EvaluateConcat(re.EvaluateHash(nil), re.EvaluateHash("x")))
	second := re.EvaluateHash(re.EvaluateConcat(re.EvaluateHash("x"), re.EvaluateHash(nil)))

	if first == second {
		t.Errorf("(absent, \"x\") and (\"x\", absent) derive the SAME id: %s\n\n"+
			"That is memql#3009's aliasing, reached through a zero-width digest instead of a "+
			"separator. dsl/cognition/logic.memql derives an id from two event-payload fields "+
			"that can each be absent, and evaluates through this path.", first)
	}
}
