package memql

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// cond_comparison_predicate_2962_test.go -- memql#2962.
//
// A `cond` whose predicate is a COMPARISON over an arg -- rather than a bare
// arg ref -- was a constant. It always took the else branch:
//
//	logic roleGate(role: "owner")  -> "plain"   <-- wrong, and silent
//	logic roleGate(role: "reader") -> "plain"
//
// for `return cond(args.role == "owner", "elevated", "plain")`. It loaded
// green and `memqllint dsl/` reported nothing, so **any role gate written this
// way was open or closed by accident, never by the role** -- and
// authoring-rules section 11b blesses exactly that shape.
//
// # Mechanism
//
// The predicate parses to a ComparisonExpression carrying a
// FieldReference{Raw: "args.role"}, NOT an *ArgRefExpression. So
// substituteArgRefValue never rewrites it (it walks arg-ref nodes only), and
// the only thing left to resolve it is walkLambdaFieldRef -- which consulted
// the lambda `locals` map alone. `locals` has no "args" key, so the LHS
// resolved to nil and `nil == "owner"` is false for every input.
//
// # Why these tests are shaped this way
//
// They drive the REAL parse -> expand -> evaluate chain via
// tryParseNewFunctionSyntax, not a hand-built AST. That is the whole reason
// this survived: the seam tests in cond_argref_test.go construct
// `&ArgRefExpression{}` operands directly, so they never produce the
// ComparisonExpression the parser actually emits. A hand-built test cannot see
// this bug.
//
// The distinguishing assertion is always "two different inputs give two
// different answers". A test that only checked the true branch would pass
// against the broken code for the `!=` and `<` shapes, where the constant
// else-branch happens to be the expected answer.

// condProbeSource builds a single-statement logic whose body is `expr`.
func condProbeSource(expr string) string {
	return fmt.Sprintf(`@enabled
@description("memql#2962 predicate-shape probe")
logic condProbe {
  args {
    role  string   @required
    n     int      @required
    flag  boolean  @required
  }
  body {
    return %s
  }
}
`, expr)
}

// evalCondProbe parses `expr` as a logic body and evaluates it against args,
// through the same chain a caller drives.
func evalCondProbe(t *testing.T, expr string, args map[string]any) any {
	t.Helper()
	fn, err := tryParseNewFunctionSyntax("condProbe", "logic", condProbeSource(expr), "memql#2962-test", memorynodes.DefaultRegistry())
	require.NoErrorf(t, err, "the probe body %q must parse", expr)

	v := newFunctionValidatorWithOrigin(nil, nil, 0)
	out, err := v.expandExpressionWithArgs(fn.Expr, args)
	require.NoErrorf(t, err, "expanding %q", expr)

	got, err := evalCollScalar(out, args, nil)
	require.NoErrorf(t, err, "evaluating %q", expr)
	return got
}

// TestCond_ComparisonPredicateOverArgsDiscriminates is the reproduction.
//
// Every operator gets two inputs that must yield DIFFERENT answers. Against
// the unfixed code every row returns the else branch twice.
func TestCond_ComparisonPredicateOverArgsDiscriminates(t *testing.T) {
	hi := map[string]any{"role": "owner", "n": 10, "flag": true}
	lo := map[string]any{"role": "reader", "n": 1, "flag": false}

	for _, tc := range []struct {
		name   string
		expr   string
		wantHi any
		wantLo any
	}{
		{"eq", `cond(args.role == "owner", "elevated", "plain")`, "elevated", "plain"},
		{"ne", `cond(args.role != "owner", "elevated", "plain")`, "plain", "elevated"},
		{"gt", `cond(args.n > 5, "elevated", "plain")`, "elevated", "plain"},
		{"ge", `cond(args.n >= 5, "elevated", "plain")`, "elevated", "plain"},
		{"lt", `cond(args.n < 5, "elevated", "plain")`, "plain", "elevated"},
		{"le", `cond(args.n <= 5, "elevated", "plain")`, "plain", "elevated"},

		// The predicate must still work when the cond is not the root node --
		// the resolution happens per-node, but a wrapper is where a partial
		// fix would show up.
		{"nested in coalesce", `coalesce(cond(args.role == "owner", "elevated", "plain"), "fallback")`, "elevated", "plain"},
		{"nested in concat", `concat("role-", cond(args.role == "owner", "elevated", "plain"))`, "role-elevated", "role-plain"},

		// memql#2915's shape, kept here so a change to the arg-ref path that
		// breaks it is caught next to its sibling.
		{"bare arg-ref predicate", `cond(args.flag, "elevated", "plain")`, "elevated", "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotHi := evalCondProbe(t, tc.expr, hi)
			gotLo := evalCondProbe(t, tc.expr, lo)

			require.Equalf(t, tc.wantHi, gotHi,
				"%s: wrong branch for the high input.\nA comparison predicate parses to a "+
					"ComparisonExpression over FieldReference{Raw:\"args.X\"}, which "+
					"substituteArgRefValue does not rewrite; if the field walk cannot reach "+
					"the args map the LHS is nil and the cond is a constant (memql#2962).", tc.expr)
			require.Equalf(t, tc.wantLo, gotLo, "%s: wrong branch for the low input", tc.expr)

			// The load-bearing one. Both branches being reachable is what
			// makes this a gate rather than a constant, and it is what the
			// unfixed code violates for every row above.
			require.NotEqualf(t, gotHi, gotLo,
				"%s returned %#v for BOTH inputs -- the predicate is not being evaluated "+
					"against the supplied args, so the gate is open or closed by accident "+
					"rather than by its condition (memql#2962).", tc.expr, gotHi)
		})
	}
}

// TestCond_UnsupportedPredicateShapesAreLoadErrors is memql#2962's fourth
// definition-of-done item: a shape that cannot be supported must fail at LOAD,
// never evaluate to a silent constant.
//
// Measured rather than assumed -- all three already refuse, and this pins that
// they keep refusing. A future change that made `&&` inside a cond arg parse
// without also evaluating it would recreate exactly the defect this file is
// about, one connective over.
func TestCond_UnsupportedPredicateShapesAreLoadErrors(t *testing.T) {
	for _, tc := range []struct{ name, expr string }{
		{"conjunction", `cond(args.role == "owner" && args.n > 5, "elevated", "plain")`},
		{"disjunction", `cond(args.role == "owner" || args.n > 5, "elevated", "plain")`},
		{"negation", `cond(!(args.role == "owner"), "elevated", "plain")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tryParseNewFunctionSyntax("condProbe", "logic", condProbeSource(tc.expr), "memql#2962-test", memorynodes.DefaultRegistry())
			require.Errorf(t, err,
				"%q must be refused at load. Accepting it without evaluating it is the "+
					"memql#2962 defect: a predicate that parses but never discriminates is a "+
					"silent constant, and a gate written on it is open or closed by accident.",
				tc.expr)
		})
	}
}

// TestExecute_CondComparisonPredicate_RunsAndDiscriminates is the end-to-end
// guard memql#2962 asks for by name: "a test driving it through
// MemQLEngine.Execute, not evalCollScalar -- the seam tests cannot see this,
// which is how it survived."
//
// Postgres-gated (Execute needs a configured DB), so it skips locally without
// one and runs in CI. The DB-free tests above cover the same chain from the
// parser down; this one covers the last hop.
func TestExecute_CondComparisonPredicate_RunsAndDiscriminates(t *testing.T) {
	eng, _, ctx := readMergeTestEngine(t)

	const src = `@enabled
@description("memql#2962 end-to-end role gate")
logic roleGate {
  args {
    role string @required
  }
  body {
    return cond(args.role == "owner", "elevated", "plain")
  }
}
`
	fn, err := tryParseNewFunctionSyntax("roleGate", "logic", src, "memql#2962-test", memorynodes.DefaultRegistry())
	require.NoError(t, err, "the role-gate shape must parse")
	require.NoError(t, eng.Functions().Upsert(fn))

	call := func(role string) any {
		raw, mErr := json.Marshal(role)
		require.NoError(t, mErr)
		res, eErr := eng.Execute(ctx, "logic roleGate(role: "+string(raw)+")")
		require.NoErrorf(t, eErr, "roleGate(%q) must run to completion", role)
		require.NotNil(t, res)
		return res.OutputPayload()
	}

	owner, reader := call("owner"), call("reader")
	require.Equal(t, "elevated", owner, "an owner must take the then branch")
	require.Equal(t, "plain", reader, "a reader must take the else branch")
	require.NotEqual(t, owner, reader,
		"roleGate returned the same value for both roles through Execute -- the gate never "+
			"fires (memql#2962)")
}
