package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// cond_ambient_envelope_coverage_3024_test.go -- memql#3024: a conditional
// whose predicate reads an ambient root (`actor.` / `config.` / `partition` /
// `now`) must discriminate. Resolving to nothing, it silently takes the else
// branch, and a gate written on it is open or closed by accident. These tests
// also close the gaps the landing review found in the first cut of the fix.
//
// The tests here answer the same question from different sides: WHICH
// ambient paths actually resolve, and does anything notice when one stops?
// The first cut answered it three different ways in three places -- isAmbientRoot
// accepted five roots, buildAmbientEnvelope supplied four, and the LogicRunner
// bound one -- so a predicate could be accepted by the validator, folded to nil
// by expansion, and silently take the else branch forever. That is memql#2962's
// defect reached through memql#3024's own fix. A logic's statement body runs on
// the LogicRunner, which binds the ambient roots from the same canonical
// envelope, so the evaluation tests run over that envelope.

// TestAmbientRootsMatchEnvelopeKeys is the drift test, and it is the durable
// half of the fix -- correcting the list without pinning it just resets the
// clock on the next divergence.
//
// component/automations/logic_runner.go records the precedent verbatim: "The
// list had drifted from two others that describe the same set ... which is why
// nobody noticed one of the three was short" (#2818 / #2851), which shipped as
// deploy role gates denying every role including owner. isAmbientRoot was the
// same bug's next instance -- it accepted `trace`, which no envelope has ever
// supplied.
func TestAmbientRootsMatchEnvelopeKeys(t *testing.T) {
	envelope := buildAmbientEnvelope(context.Background(), nil)

	for key := range envelope {
		require.Truef(t, isAmbientRoot(key),
			"buildAmbientEnvelope supplies %q but isAmbientRoot rejects it, so a cond predicate "+
				"rooted there is never resolved during arg expansion and silently takes the else "+
				"branch. Add %q to ambientEnvelopeRoots.", key, key)
	}

	for root := range ambientEnvelopeRoots {
		_, ok := envelope[root]
		require.Truef(t, ok,
			"isAmbientRoot accepts %q but buildAmbientEnvelope never supplies it, so "+
				"substituteArgRefValue folds %q.* to nil and the comparison becomes a CONSTANT -- "+
				"the memql#2962 silent gate, reintroduced. Either supply the key or drop %q from "+
				"ambientEnvelopeRoots.", root, root, root)
	}
}

// TestLogicAmbientPredicate_RejectsUnresolvableActorPaths pins the ambient
// paths a logic's load refuses here: an actor member the auth envelope does
// not carry (the closed-set check, #2623). Documented and unresolvable is the
// worst combination -- CLAUDE.md's argument-resolution table once listed
// actor.partitions -- so an author has every reason to write it, and at run
// time it would read absent.
//
// The other two unresolvable ambient paths are refused elsewhere: `trace` (a
// reserved root no envelope supplies) by the body's own scope check
// (body_unknown_name, component/language/compiler's body_scope_test.go), and
// a `config` key outside the allow-list by the boot gate
// (body_config_unknown, dslgate's statement_bodies_test.go).
func TestLogicAmbientPredicate_RejectsUnresolvableActorPaths(t *testing.T) {
	for name, tc := range map[string]struct{ pred, wants string }{
		"actor-unknown-leaf": {
			pred:  `actor.partitions == "p"`,
			wants: "unknown actor member",
		},
		"actor-typo": {
			pred:  `actor.rol == "owner"`,
			wants: "unknown actor member",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tryParseNewFunctionSyntax("ambientPathProbe", "logic", condAmbientProbeSource("ambientPathProbe", tc.pred), "common.logic.memql", dotAccessLoadRegistry())
			require.Errorf(t, err,
				"(%s) ? ... resolves to nothing on every evaluation path, so it is a CONSTANT "+
					"that takes the else branch for every input. It must be refused at load (memql#2962).",
				tc.pred)
			require.Contains(t, err.Error(), tc.wants)
		})
	}
}

// TestCondAmbientConfigPredicate_DiscriminatesOverTheEnvelope is memql#3024's
// definition-of-done bullet 2 for the half that was never demonstrated: the
// bullet names `config.X` as explicitly as `actor.X`.
//
// The first cut's only config assertion was a load-time check on a key that
// does not exist, so it would have passed with config resolution entirely
// removed. This one cannot: it evaluates the same predicate under snapshots
// that differ and requires the answers to differ. The envelope is the
// engine's canonical one (buildAmbientEnvelope), which the LogicRunner binds
// `config` from; the runner itself is component/automations'.
func TestCondAmbientConfigPredicate_DiscriminatesOverTheEnvelope(t *testing.T) {
	run := func(t *testing.T, name, pred string, snapshot *busv1.ConfigSnapshot) any {
		t.Helper()
		eng := &MemQLEngine{}
		eng.SetConfigSnapshot(snapshot)
		return evalOverTheEnvelope(t, eng, context.Background(), name, pred, map[string]any{"a": "ignored"})
	}

	t.Run("bool", func(t *testing.T) {
		on := run(t, "cfgDemoOn", `config.demoMode == true`, &busv1.ConfigSnapshot{DemoMode: true})
		off := run(t, "cfgDemoOff", `config.demoMode == true`, &busv1.ConfigSnapshot{DemoMode: false})
		require.Equal(t, "elevated", on)
		require.Equal(t, "plain", off)
		require.NotEqualf(t, on, off,
			"(config.demoMode == true) ? ... returned %#v under BOTH snapshots, so the "+
				"predicate is not evaluated against the allow-listed config surface and the gate "+
				"is a constant (memql#3024 DoD bullet 2).", on)
	})

	t.Run("string", func(t *testing.T) {
		match := run(t, "cfgProviderMatch", `config.defaultProvider == "chat54Mini"`,
			&busv1.ConfigSnapshot{SiDefaultProvider: "chat54Mini"})
		miss := run(t, "cfgProviderMiss", `config.defaultProvider == "chat54Mini"`,
			&busv1.ConfigSnapshot{SiDefaultProvider: "somethingElse"})
		require.Equal(t, "elevated", match)
		require.Equal(t, "plain", miss)
	})

	// `partition` and `now` ride the same envelope as `config`. Both are
	// single-segment roots, so they also exercise the bare-root reads that
	// the dotted paths never reach. The values are environment-dependent, so
	// the assertion is on DISCRIMINATION rather than on a literal: each
	// predicate and its negation must give opposite answers. A constant
	// cannot do that.
	t.Run("single-segment-roots", func(t *testing.T) {
		for name, root := range map[string]string{"partition": "partition", "now": "now"} {
			t.Run(name, func(t *testing.T) {
				empty := run(t, "amb"+name+"Empty", root+` == ""`, nil)
				nonEmpty := run(t, "amb"+name+"NonEmpty", root+` != ""`, nil)
				require.NotEqualf(t, empty, nonEmpty,
					"`%s == \"\"` and `%s != \"\"` both returned %#v, so %q is not resolved "+
						"and any gate over it is a constant (memql#3024).",
					root, root, empty, root)
			})
		}
	})
}

// TestCondAmbientPredicate_NegatedAbsentActorDeniesOverTheEnvelope pins the
// memql#2801 guarantee -- and it is the one that discriminates, where its
// sibling does not.
//
// `actor.isClusterOwner == true` denies on an absent actor whether the
// envelope is built or not, because an unresolved predicate also falls to the
// else branch: the expected value is identical under the fix and under the
// bug, so that assertion is blind. Comparing against the DENYING VALUE is
// what separates them. With the envelope built unconditionally,
// `isClusterOwner != false` is false and the gate denies; with the key absent,
// `!=` reads TRUE, and an "if you ARE an owner" gate opens for an
// unauthenticated caller.
func TestCondAmbientPredicate_NegatedAbsentActorDeniesOverTheEnvelope(t *testing.T) {
	for name, pred := range map[string]string{
		"isClusterOwner": `actor.isClusterOwner != false`,
		"role":           `actor.role != ""`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, hasAccess := auth.AccessFromContext(ctx)
			require.False(t, hasAccess, "this test is only meaningful with no resolved actor")

			got := evalOverTheEnvelope(t, &MemQLEngine{}, ctx, "ambientNegatedDenyGate"+name, pred, map[string]any{"a": "x"})
			require.Equalf(t, "plain", got,
				"(%s) ? ... took the THEN branch with no authenticated caller. That is the "+
					"memql#2801 fail-open: the envelope must be built unconditionally so every key "+
					"is present with a denying value, because an ABSENT key makes a negated "+
					"predicate read true and opens the gate for an unauthenticated caller.", pred)
		})
	}
}

// condAmbientProbeSource builds a logic returning a conditional over `pred`.
func condAmbientProbeSource(name, pred string) string {
	return strings.Join([]string{
		"@enabled",
		"@actor",
		"@description(\"memql#3024 ambient predicate probe\")",
		"logic " + name + " {",
		"  args {",
		"    a string @required",
		"  }",
		"  return (" + pred + ") ? \"elevated\" : \"plain\"",
		"}",
	}, "\n")
}

// evalOverTheEnvelope evaluates a probe logic's returned expression the way
// the LogicRunner evaluates it for the ambient roots: with EvalExpr, over the
// engine's canonical envelope for ctx (buildAmbientEnvelope -- the source the
// runner binds config and partition from, and whose actor is the same
// auth.ActorEnvelopeMap the runner binds) beside the call's arguments.
func evalOverTheEnvelope(t *testing.T, eng *MemQLEngine, ctx context.Context, name, pred string, args map[string]any) any {
	t.Helper()
	fn, err := tryParseNewFunctionSyntax(name, "logic", condAmbientProbeSource(name, pred), "memql#3024-test", memorynodes.DefaultRegistry())
	require.NoErrorf(t, err, "an ambient predicate must LOAD: %s", pred)
	scope := MapScope{"args": args}
	for k, v := range eng.AmbientEnvelope(ctx) {
		scope[k] = v
	}
	got, err := EvalExpr(ctx, statementReturnExpr(t, fn), scope, EvalOptions{})
	require.NoErrorf(t, err, "evaluating %s", pred)
	return got
}

// TestCondAmbientPredicate_DiscriminatesOverTheEnvelope: an ambient predicate
// is evaluated against the resolved actor, so an owner and a reader take
// different branches. (Before edition 2026 this was driven through
// MemQLEngine.Execute, because the pre-2026 path expanded the arguments and
// then evaluated the plan root with no arguments at all; a logic's body runs
// on the LogicRunner, which component/automations provides and drives end to
// end in its logic corpus.)
func TestCondAmbientPredicate_DiscriminatesOverTheEnvelope(t *testing.T) {
	eng := &MemQLEngine{}
	call := func(role string) any {
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u-" + role, Role: auth.Role(role)})
		return evalOverTheEnvelope(t, eng, ctx, "ambientRoleGate", `actor.role == "owner"`, map[string]any{"a": "ignored"})
	}
	owner, reader := call("owner"), call("reader")
	require.Equal(t, "elevated", owner, "an owner actor must take the then branch")
	require.Equal(t, "plain", reader, "a reader actor must take the else branch")
	require.NotEqualf(t, owner, reader,
		"ambientRoleGate returned %#v for BOTH actors -- the predicate is not evaluated against the "+
			"resolved actor envelope, so the gate is open or closed by accident (memql#3024).", owner)
}

// TestCondAmbientPredicate_AbsentActorDeniesOverTheEnvelope pins the
// fail-closed direction.
//
// buildAmbientEnvelope is built UNCONDITIONALLY (memql#2801): an absent auth
// context yields the DENYING envelope with every key present, rather than an
// empty map whose absent keys make a negated predicate evaluate true. A gate
// that opens when authentication is missing is worse than one that never fires.
func TestCondAmbientPredicate_AbsentActorDeniesOverTheEnvelope(t *testing.T) {
	// context.Background() carries no AccessContext, so the envelope is the
	// denying default.
	got := evalOverTheEnvelope(t, &MemQLEngine{}, context.Background(), "ambientDenyGate", `actor.isClusterOwner == true`, map[string]any{"a": "x"})
	require.Equal(t, "plain", got,
		"with no resolved actor the owner gate must DENY. An envelope that omits keys instead "+
			"of defaulting them is the memql#2801 fail-open: the predicate compares against a "+
			"missing value and a gate written this way opens for an unauthenticated caller.")
}
