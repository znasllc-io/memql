package memql

import (
	"context"
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
//
// # In edition 2026
//
// cond() is retired: the conditional is `p ? a : b`, and a logic's return
// statement is evaluated by the LogicRunner with EvalExpr over the call's
// arguments. The mechanism above has no edition-2026 path. What stays is the
// property -- a comparison predicate over an argument discriminates -- driven
// through the real parse and compile and the evaluator the runner calls, with
// the two-inputs assertion.

// condProbeSource builds a single-statement logic whose body is `expr`.
func condProbeSource(expr string) string {
	return fmt.Sprintf(`
@description("memql#2962 predicate-shape probe")
logic condProbe {
  args {
    role  string   @required
    n     int      @required
    flag  boolean  @required
  }
  return %s
}
`, expr)
}

// evalCondProbe loads `expr` as a logic body and evaluates its return
// statement's value against args with EvalExpr, the evaluator the LogicRunner
// runs it with.
func evalCondProbe(t *testing.T, expr string, args map[string]any) any {
	t.Helper()
	fn, err := tryParseNewFunctionSyntax("condProbe", "logic", condProbeSource(expr), "memql#2962-test", memorynodes.DefaultRegistry())
	require.NoErrorf(t, err, "the probe body %q must parse", expr)

	got, err := EvalExpr(context.Background(), statementReturnExpr(t, fn), MapScope{"args": args}, EvalOptions{})
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
		{"eq", `args.role == "owner" ? "elevated" : "plain"`, "elevated", "plain"},
		{"ne", `args.role != "owner" ? "elevated" : "plain"`, "plain", "elevated"},
		{"gt", `args.n > 5 ? "elevated" : "plain"`, "elevated", "plain"},
		{"ge", `args.n >= 5 ? "elevated" : "plain"`, "elevated", "plain"},
		{"lt", `args.n < 5 ? "elevated" : "plain"`, "plain", "elevated"},
		{"le", `args.n <= 5 ? "elevated" : "plain"`, "plain", "elevated"},

		// The predicate must still work when the conditional is not the root
		// node -- a wrapper is where a partial fix would show up.
		{"nested in coalesce", `(args.role == "owner" ? "elevated" : "plain") ?? "fallback"`, "elevated", "plain"},
		{"nested in a join", `"role-" + (args.role == "owner" ? "elevated" : "plain")`, "role-elevated", "role-plain"},

		// memql#2915's shape, kept here so a change to the arg-ref path that
		// breaks it is caught next to its sibling.
		{"bare arg-ref predicate", `args.flag ? "elevated" : "plain"`, "elevated", "plain"},
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

// TestCond_CompoundPredicatesDiscriminate is memql#2962's fourth
// definition-of-done item in edition 2026. Before the flip a conjunction,
// disjunction or negation inside a cond predicate could not be evaluated, so
// each was refused at LOAD rather than evaluate to a silent constant. The
// one grammar evaluates them -- a condition is `&&`, `||` or `!` over
// booleans, in every position -- so they load, and the two-inputs assertion
// shows they discriminate rather than fold to a constant.
func TestCond_CompoundPredicatesDiscriminate(t *testing.T) {
	hi := map[string]any{"role": "owner", "n": 10, "flag": true}
	lo := map[string]any{"role": "reader", "n": 1, "flag": false}
	for _, tc := range []struct {
		name, expr     string
		wantHi, wantLo any
	}{
		{"conjunction", `args.role == "owner" && args.n > 5 ? "elevated" : "plain"`, "elevated", "plain"},
		{"disjunction", `args.role == "owner" || args.n > 5 ? "elevated" : "plain"`, "elevated", "plain"},
		{"negation", `!(args.role == "owner") ? "elevated" : "plain"`, "plain", "elevated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotHi, gotLo := evalCondProbe(t, tc.expr, hi), evalCondProbe(t, tc.expr, lo)
			require.Equal(t, tc.wantHi, gotHi, "%s: wrong branch for the high input", tc.expr)
			require.Equal(t, tc.wantLo, gotLo, "%s: wrong branch for the low input", tc.expr)
			require.NotEqual(t, gotHi, gotLo, "%s returned the same value for both inputs: a silent constant (memql#2962)", tc.expr)
		})
	}
}
