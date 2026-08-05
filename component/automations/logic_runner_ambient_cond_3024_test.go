package automations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	"github.com/znasllc-io/memql/component/memql"
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
// For `actor.*` it resolves. The runner binds the actor envelope
// unconditionally (newEvaluatorForLogic, #2380 / #2818 / #2801 / #2623), so
// the refusal was OVER-BROAD for that root -- it rejected a shape this path
// already handled correctly. This test is what keeps that true, because
// nothing else covers the ambient predicate position here: the deleted
// validator meant it could never be reached, so there was no reason to test it
// before.
//
// BE HONEST ABOUT WHAT THIS TEST IS. It is a characterization test: no
// production code in this package changed for the `actor` case, so it passes
// unmodified on origin/main too and cannot fail-first. It earns its place by
// guarding behaviour that only became REACHABLE when the load refusal was
// deleted, not by proving a fix. The test that does fail-first on this path is
// TestLogicRunner_AmbientCondConfigPredicate_Discriminates below -- and the
// reason it exists is that "the runner resolves ambient" was not true for
// every root when it was first written.
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

// TestLogicRunner_AmbientCondConfigPredicate_Discriminates is the half the
// first cut of memql#3024 got wrong on this path, and unlike its siblings above
// it fails against that cut.
//
// The claim "the multi-step path already resolves ambient, so the deleted
// refusal was over-broad" was true for `actor` and false for everything else:
// newEvaluatorForLogic bound the actor envelope and nothing more, so
// `cond(config.demoMode == true, ...)` here resolved against nothing and
// returned the else branch for every snapshot -- the memql#2962 silent
// constant, reached by deleting the load error that had made the shape
// unreachable. The generalisation was drawn from two `actor.*` cases.
//
// `partition` and `now` ride the same binding; config is the one with two
// distinguishable states to assert on, which is what makes it the test.
func TestLogicRunner_AmbientCondConfigPredicate_Discriminates(t *testing.T) {
	eval := func(t *testing.T, snapshot *busv1.ConfigSnapshot, expr string) any {
		t.Helper()
		engine := &memql.MemQLEngine{}
		engine.SetConfigSnapshot(snapshot)
		runner := NewLogicRunner(engine, nil, nil)

		val, handled, err := tryEvaluateBuiltinLocally(
			expr, runner.newEvaluatorForLogic(context.Background(), map[string]any{"a": "x"}))
		require.NoErrorf(t, err, "evaluating %q", expr)
		require.Truef(t, handled,
			"%q must be handled locally by the runner's positional-builtin path; if it stops "+
				"being handled the predicate falls through to a step lookup and the cond takes "+
				"its fallback for every snapshot (the memql#2818 failure mode)", expr)
		return val
	}

	t.Run("bool", func(t *testing.T) {
		expr := `cond(config.demoMode == true, "elevated", "plain")`
		on := eval(t, &busv1.ConfigSnapshot{DemoMode: true}, expr)
		off := eval(t, &busv1.ConfigSnapshot{DemoMode: false}, expr)

		require.Equal(t, "elevated", on)
		require.Equal(t, "plain", off)
		require.NotEqualf(t, on, off,
			"%s returned %#v under BOTH snapshots -- the multi-step evaluator does not bind the "+
				"allow-listed config surface, so the gate is a constant. The load refusal that "+
				"used to hide this is deleted (memql#3024).", expr, on)
	})

	t.Run("string", func(t *testing.T) {
		expr := `cond(config.defaultProvider == "chat54Mini", "elevated", "plain")`
		match := eval(t, &busv1.ConfigSnapshot{SiDefaultProvider: "chat54Mini"}, expr)
		miss := eval(t, &busv1.ConfigSnapshot{SiDefaultProvider: "somethingElse"}, expr)

		require.Equal(t, "elevated", match)
		require.Equal(t, "plain", miss)
	})

	// A nil engine binds nothing rather than an empty envelope -- there is no
	// config state to speak for without one. Pinned so the guard in
	// bindEngineAmbientEnvelope is not quietly removed as dead.
	t.Run("nil-engine-does-not-panic", func(t *testing.T) {
		runner := NewLogicRunner(nil, nil, nil)
		_, _, err := tryEvaluateBuiltinLocally(
			`cond(config.demoMode == true, "elevated", "plain")`,
			runner.newEvaluatorForLogic(context.Background(), map[string]any{}))
		require.NoError(t, err)
	})
}
