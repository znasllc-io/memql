package automations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// logic_runner_ambient_cond_3024_test.go -- the MULTI-STEP half of memql#3024.
//
// #3024 threads the ambient envelope through arg expansion so a single-
// statement `return cond(actor.X == ..., ...)` evaluates, and deletes
// validateLogicCondAmbientPredicate, which had refused that shape at load.
//
// That validator walked EVERY step, so it also refused an ambient cond
// predicate in a MULTI-STEP body -- and a multi-step body does not go through
// arg expansion at all. It compiles to plan.LogicCall and runs here, on the
// LogicRunner's own evaluator. So deleting the refusal raises a question the
// expansion-side fix cannot answer: does this path resolve ambient, or did the
// load error just trade a loud failure for a silent constant?
//
// It resolves. The runner binds `actor.*` unconditionally
// (newEvaluatorForLogic, #2380 / #2818 / #2801 / #2623), so the refusal was
// OVER-BROAD -- it rejected a shape this path already handled correctly. This
// test is what keeps that true, because nothing else covers the ambient
// predicate position here: the deleted validator meant it could never be
// reached, so there was no reason to test it before.
//
// The distinguishing assertion is always "two different actors give two
// different answers". Checking only the owner case would pass against a
// constant that happens to return the then branch.
func TestLogicRunner_AmbientCondPredicateDiscriminatesByActor(t *testing.T) {
	runner := NewLogicRunner(nil, nil, nil)

	eval := func(role, expr string) any {
		t.Helper()
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "u-" + role,
			Role:   auth.Role(role),
		})
		val, handled, err := tryEvaluateBuiltinLocally(expr, runner.newEvaluatorForLogic(ctx, map[string]any{"a": "x"}))
		require.NoErrorf(t, err, "evaluating %q for role %q", expr, role)
		require.Truef(t, handled,
			"%q must be handled locally by the runner's positional-builtin path; if it stops "+
				"being handled the predicate falls through to a step lookup and the cond takes "+
				"its fallback for every actor (the memql#2818 failure mode)", expr)
		return val
	}

	for name, expr := range map[string]string{
		"role":           `cond(actor.role == "owner", "elevated", "plain")`,
		"isClusterOwner": `cond(actor.isClusterOwner == true, "elevated", "plain")`,
	} {
		t.Run(name, func(t *testing.T) {
			owner := eval("owner", expr)
			reader := eval("reader", expr)

			require.Equal(t, "elevated", owner, "an owner actor must take the then branch")
			require.Equal(t, "plain", reader, "a reader actor must take the else branch")
			require.NotEqualf(t, owner, reader,
				"%s returned %#v for BOTH actors -- the ambient predicate is not evaluated "+
					"against the bound actor envelope, so the gate is open or closed by accident "+
					"rather than by the role (memql#3024).", expr, owner)
		})
	}
}

// TestLogicRunner_AmbientCondPredicateDeniesWithoutActor is the fail-closed
// direction, and the reason the runner binds the envelope unconditionally
// (#2801): an unbound `actor.*` used to render as its own path text, which is
// a non-empty and therefore TRUTHY string -- fail-open on the one field that
// gates admin work.
func TestLogicRunner_AmbientCondPredicateDeniesWithoutActor(t *testing.T) {
	runner := NewLogicRunner(nil, nil, nil)

	// context.Background() carries no AccessContext.
	val, handled, err := tryEvaluateBuiltinLocally(
		`cond(actor.isClusterOwner == true, "ALLOW", "DENY")`,
		runner.newEvaluatorForLogic(context.Background(), map[string]any{}))
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, "DENY", val,
		"with no resolved actor the owner gate must DENY. An unbound actor path renders as its "+
			"own text, which is truthy -- fail-open on the field that gates admin work (#2380 / "+
			"#2801). ActorEnvelopeMap denies for a nil context, and binding it always is what "+
			"keeps that true.")
}
