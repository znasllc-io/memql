package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// cond_quoted_string_3618_test.go -- memql#3618.
//
// `cond()` in a mutation payload took the WRONG BRANCH on any quoted-string
// comparison. evalCondition split the predicate text on `==` / `!=` by hand and
// ran each half through evalString, which returns a quoted literal WITH ITS
// QUOTES ATTACHED (the "Treat as literal string" fall-through). So `"active"`
// was compared as the 8-character string `"active"` against the 6-character
// value `active`:
//
//	evalCondition(`args.s == "active"`)  with s="active"  -> false   (want true)
//	evalCondition(`args.s != "active"`)  with s="active"  -> true    (want false)
//
// Quoting a string comparison is what any author would write, so the NATURAL
// spelling was the broken one. Nothing in the live tree tripped it -- the two
// mutation-body cond() uses are bare-truthiness -- which is exactly why it
// stayed loaded and silent.
//
// The fix routes the predicate through the canonical expression parser and the
// lowered-AST evaluator, so there is ONE implementation of the operator instead
// of two that disagree. The hand-rolled splitter is gone. `*ComparisonExpr` --
// the field-vs-value shape the parser actually emits for an identifier-led
// comparison -- is now handled by evalParserExpression alongside the
// expression-led `*BinaryComparisonExpr` it already had; the lexer strips the
// quotes, so a literal arrives as a literal.
//
// Relational operators come along for free: `cond(args.n > 5, ...)` used to die
// with `args.<path>: invalid character ' '` because the splitter handed the
// whole `args.n > 5` text to the arg-path resolver.

func newCondEvaluator(args map[string]any) *mutationTemplateEvaluator {
	return &mutationTemplateEvaluator{args: args}
}

// TestEvalCondition_QuotedStringComparison is the reproduction, at the seam.
func TestEvalCondition_QuotedStringComparison(t *testing.T) {
	args := map[string]any{
		"s":     "active",
		"other": "active",
		"n":     int64(5),
		"blank": "",
	}
	for _, tc := range []struct {
		name string
		expr string
		want bool
	}{
		{"quoted eq matches", `args.s == "active"`, true},
		{"quoted ne matches", `args.s != "active"`, false},
		{"quoted eq on the left", `"active" == args.s`, true},
		{"quoted eq mismatch", `args.s == "archived"`, false},
		{"quoted ne mismatch", `args.s != "archived"`, true},
		{"missing arg equals empty literal", `args.missing == ""`, true},
		{"blank arg equals empty literal", `args.blank == ""`, true},

		// Shapes that were already correct and must stay correct.
		{"unquoted eq", `args.s == active`, true},
		{"numeric eq", `args.n == 5`, true},
		{"arg vs arg", `args.s == args.other`, true},
		{"boolean literal true", `true`, true},
		{"boolean literal false", `false`, false},
		{"bare truthy arg", `args.s`, true},
		{"bare falsy arg", `args.blank`, false},
		{"bare missing arg", `args.missing`, false},

		// Relationals: previously an ERROR out of the arg-path resolver.
		{"gt true", `args.n > 4`, true},
		{"gt false", `args.n > 5`, false},
		{"ge true", `args.n >= 5`, true},
		{"lt true", `args.n < 6`, true},
		{"le false", `args.n <= 4`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := newCondEvaluator(args).evalCondition(context.Background(), tc.expr)
			require.NoErrorf(t, err, "evalCondition(%q)", tc.expr)
			require.Equalf(t, tc.want, got,
				"evalCondition(%q) took the wrong branch. A quoted literal must compare as its "+
					"CONTENT, not with its quotes attached (memql#3618).", tc.expr)
		})
	}
}

// TestCondInMutationPayload_QuotedStringComparison drives the whole path a
// mutation body takes: the insert-block object literal is lowered exactly as
// the loader lowers it, then rendered. This is the position from which the
// string evaluator was the only reachable one.
func TestCondInMutationPayload_QuotedStringComparison(t *testing.T) {
	payload := parseObjectLiteral(`{
		verdict: cond(args.status == "active", "MATCH", "NOMATCH"),
		inverse: cond(args.status != "active", "MATCH", "NOMATCH"),
		relational: cond(args.count > 5, "BIG", "SMALL")
	}`)
	require.NotNil(t, payload, "the insert-block literal must lower")

	e := &MemQLEngine{} // concepts nil -> canonicalize is a no-op
	node, err := e.renderMutationTemplate(context.Background(), &FunctionMutationTemplate{
		Concept:         "v1:cognition:space",
		PayloadTemplate: payload,
	}, map[string]any{"status": "active", "count": int64(9)})
	require.NoError(t, err,
		"a relational cond() predicate used to fail here with "+
			`"args.<path>: invalid character ' '" (memql#3618)`)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(node.PayloadRaw), &got))

	require.Equal(t, "MATCH", got["verdict"],
		`cond(args.status == "active", ...) with status="active" must take the THEN branch `+
			"(memql#3618)")
	require.Equal(t, "NOMATCH", got["inverse"],
		`cond(args.status != "active", ...) with status="active" must take the ELSE branch`)
	require.Equal(t, "BIG", got["relational"], "cond(args.count > 5, ...) with count=9 is BIG")
}

// TestCondPredicate_OneEvaluator pins the unification itself: the string
// spelling and the lowered-AST spelling of the same predicate must agree.
//
// memql#3618's headline was that they did NOT -- the AST path returned MATCH
// and the string path NOMATCH for the identical predicate -- and this file's
// history (memql#2840, memql#2925) is a list of exactly that drift going
// unnoticed until a construct landed in the other slot.
func TestCondPredicate_OneEvaluator(t *testing.T) {
	args := map[string]any{"s": "active", "n": int64(5)}
	ctx := context.Background()

	for _, src := range []string{
		`cond(args.s == "active", "MATCH", "NOMATCH")`,
		`cond(args.s != "active", "MATCH", "NOMATCH")`,
		`cond(args.n > 4, "MATCH", "NOMATCH")`,
		`cond(args.n == 5, "MATCH", "NOMATCH")`,
	} {
		t.Run(src, func(t *testing.T) {
			e := newCondEvaluator(args)

			viaString, err := e.evalString(ctx, src)
			require.NoError(t, err)

			expr, perr := languageParser.ParseExpression(src)
			require.NoError(t, perr)
			viaAST, err := e.evalParserExpression(ctx, expr)
			require.NoError(t, err,
				"the lowered-AST evaluator must handle the field-vs-value *ComparisonExpr the "+
					"parser emits for an identifier-led comparison (memql#3618)")

			require.Equal(t, viaAST, viaString,
				"the two evaluators of cond() disagree on %q -- that is the memql#3618 defect", src)
		})
	}
}
