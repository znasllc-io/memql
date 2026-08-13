package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// coalesce_array_missing_3627_test.go -- memql#3627.
//
// Three write-path surprises that share a cause: the engine's notion of
// "missing" was not the same in every container, and one operator had two
// implementations.
//
//  1. `??` coalesces on empty AND WHITESPACE-ONLY strings, so a user clearing a
//     text field gets the default written back. DELIBERATE for the empty case
//     (memql#1614) and now pinned and documented here rather than left as an
//     undocumented edge -- see TestNullCoalesce_IsBlankCoalescing.
//  2. A missing arg inside an ARRAY literal became an explicit `null` element
//     while the MAP branch omitted its key. Two containers, two answers for the
//     same input. Fixed: the array omits too.
//  3. `coalesce` disagreed with itself across its two evaluators on a blank
//     middle arm ("" from the string path, nil from the lowered-AST path).
//     Fixed by giving both the SAME selection driver.

// TestNullCoalesce_IsBlankCoalescing pins what `??` actually means. The name
// says "null-coalescing"; the behaviour is BLANK-coalescing, and the whitespace
// case is the sharpest edge of it -- a field holding a single space is not
// absent by any reading, yet it is replaced.
//
// Pinned rather than changed. memql#1614 chose the empty-string behaviour
// deliberately (`f: args.f ?? ""` has to be able to land an explicit empty
// instead of a schema-failing null), the whole corpus is written against it,
// and @noUnset (memql#3415) exists as the targeted opt-out for the field where
// a caller must not be able to blank a stored value. Changing the operator
// under the corpus to fix a naming complaint would be the larger defect.
func TestNullCoalesce_IsBlankCoalescing(t *testing.T) {
	e := &mutationTemplateEvaluator{}
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		arg  any
		want any
	}{
		// Non-string zero values are KEPT: false and 0 are answers.
		{"false is kept", false, false},
		{"zero is kept", int64(0), int64(0)},
		{"empty array is kept", []any{}, []any{}},
		{"empty object is kept", map[string]any{}, map[string]any{}},

		// Strings are not. All three of these are "blank", not "absent".
		{"empty string is REWRITTEN", "", "DEFAULT"},
		{"single space is REWRITTEN", " ", "DEFAULT"},
		{"tab and newline are REWRITTEN", "\t\n", "DEFAULT"},

		// A non-blank string wins, which is the whole point of the operator.
		{"non-blank string wins", "value", "value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.args = map[string]any{"v": tc.arg}
			got, err := e.evalString(ctx, `coalesce(args.v, "DEFAULT")`)
			require.NoError(t, err)
			require.Equal(t, tc.want, got,
				"`??` is BLANK-coalescing, not null-coalescing (memql#1614, memql#3627). "+
					"A caller who deliberately cleared a text field gets the default written "+
					"back; use @noUnset (memql#3415) on the field where that must not happen.")
		})
	}
}

// TestArrayLiteral_OmitsMissingArgs is the reproduction for (2).
//
// `{ v: [args.a, args.b, "c"] }` with only b supplied produced
// `{"v":[null,"B","c"]}` -- a null HOLE the caller never sent, from the same
// input the map branch two cases up handles by omitting the key. A concept
// declaring `items: {type:"string"}` rejects the null loudly; one that does not
// stores it.
func TestArrayLiteral_OmitsMissingArgs(t *testing.T) {
	payload := parseObjectLiteral(`{ v: [args.a, args.b, "c"] }`)
	require.NotNil(t, payload)

	e := &MemQLEngine{}
	node, err := e.renderMutationTemplate(context.Background(), &FunctionMutationTemplate{
		Concept:         "v1:cognition:space",
		PayloadTemplate: payload,
	}, map[string]any{"b": "B"})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(node.PayloadRaw), &got))
	require.Equal(t, []any{"B", "c"}, got["v"],
		"a missing optional arg must contribute NOTHING to an array literal, exactly as it "+
			"contributes nothing to an object literal -- one notion of missing per engine, "+
			"not one per container (memql#3627)")
}

// TestArrayLiteral_KeepsExplicitNull is the other half: omitting a MISSING arg
// must not start omitting a null the author wrote on purpose. The map branch
// draws the same line ("Omit missing optional args. Preserve explicit nulls.").
func TestArrayLiteral_KeepsExplicitNull(t *testing.T) {
	payload := parseObjectLiteral(`{ v: [null, args.b] }`)
	require.NotNil(t, payload)

	e := &MemQLEngine{}
	node, err := e.renderMutationTemplate(context.Background(), &FunctionMutationTemplate{
		Concept:         "v1:cognition:space",
		PayloadTemplate: payload,
	}, map[string]any{"b": "B"})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(node.PayloadRaw), &got))
	require.Equal(t, []any{nil, "B"}, got["v"],
		"an explicit null is a value the author wrote; only the missing-arg sentinel is dropped")
}

// TestCoalesce_OneSelectionRule is the reproduction for (3): the string
// evaluator (payload slots) and the lowered-AST evaluator (id / createdAt /
// parent / aliasOf slots) are two implementations of one operator, and they
// disagreed on a blank middle arm -- "" versus nil.
//
// Not observable in the tree today, which is exactly the memql#2840 / memql#2925
// pattern: the divergence sits harmless until a construct lands in the other
// slot. memql#3618 is the same split producing a live wrong answer for cond().
func TestCoalesce_OneSelectionRule(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		src  string
		args map[string]any
	}{
		{"blank middle arm, nothing else resolves", `coalesce(args.a, "", args.c)`, map[string]any{}},
		{"blank middle arm, later arm wins", `coalesce(args.a, "", args.c)`, map[string]any{"c": "C"}},
		{"blank final arm is the default", `coalesce(args.a, "")`, map[string]any{}},
		{"first non-blank wins", `coalesce(args.a, "B", "C")`, map[string]any{}},
		{"present value wins", `coalesce(args.a, "B")`, map[string]any{"a": "A"}},
		{"blank present value is skipped", `coalesce(args.a, "B")`, map[string]any{"a": "  "}},
		{"all arms missing", `coalesce(args.a, args.b)`, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &mutationTemplateEvaluator{args: tc.args}

			viaString, err := e.evalString(ctx, tc.src)
			require.NoError(t, err)

			expr, perr := languageParser.ParseExpression(tc.src)
			require.NoError(t, perr)
			viaAST, err := e.evalParserExpression(ctx, expr)
			require.NoError(t, err)

			require.Equalf(t, viaAST, viaString,
				"the two evaluators of coalesce() disagree on %q with args %v (memql#3627)",
				tc.src, tc.args)
		})
	}
}
