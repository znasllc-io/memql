package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
)

// cond_ambient_envelope_coverage_3024_test.go closes the gaps the memql#3024
// landing review found in the first cut of that change.
//
// The tests here answer the same question from different sides: WHICH
// ambient paths actually resolve, and does anything notice when one stops?
// The first cut answered it three different ways in three places -- isAmbientRoot
// accepted five roots, buildAmbientEnvelope supplied four, and the LogicRunner
// bound one -- so a predicate could be accepted by the validator, folded to nil
// by expansion, and silently take the else branch forever. That is memql#2962's
// defect reached through memql#3024's own fix. Since edition 2026 a logic body
// runs on the LogicRunner, which binds the ambient roots from the same
// canonical envelope, so the evaluation tests run over that envelope.

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
				"the memql#2962 silent gate, reintroduced. Either supply the key or move %q to "+
				"reservedUnsuppliedRoots so it is refused at load.", root, root, root)
	}

	// The two sets must not overlap, or a root would be both resolvable and
	// refused depending on which check ran first.
	for root := range reservedUnsuppliedRoots {
		require.Falsef(t, isAmbientRoot(root),
			"%q is in BOTH ambientEnvelopeRoots and reservedUnsuppliedRoots", root)
		_, ok := envelope[root]
		require.Falsef(t, ok,
			"%q is listed as unsupplied but buildAmbientEnvelope supplies it", root)
	}
}

// TestLogicCondBareIdentifierPredicate_RejectsUnresolvableAmbientPaths pins
// the ambient paths an edition-2026 logic body still refuses at load: an
// actor member the auth envelope does not carry (the closed-set check, #2623).
// Documented and unresolvable is the worst combination -- CLAUDE.md's
// argument-resolution table once listed actor.partitions -- so an author has
// every reason to write it, and at run time it would read absent.
//
// The pre-2026 load also refused `trace` (a reserved root no envelope
// supplies) and a `config` key outside the allow-list. An edition-2026 body is
// not refused for either at load: `trace` is refused when the body runs
// (unknown_name), and an unlisted config key reads absent. Neither is pinned
// here; the load-time refusal is the v1 loader's to restore.
func TestLogicCondBareIdentifierPredicate_RejectsUnresolvableAmbientPaths(t *testing.T) {
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
			err := loadCondBarePredicateProbe(tc.pred)
			require.Errorf(t, err,
				"(%s) ? ... resolves to nothing on every evaluation path, so it is a CONSTANT "+
					"that takes the else branch for every input. It must be refused at load (memql#2962).",
				tc.pred)
			require.Contains(t, err.Error(), tc.wants)
		})
	}
}

// TestLogicCondBareIdentifier_MultiStepDeclaredArgIsNotRejected is the
// false-positive guard, and it is the direction that matters most on a load
// rule: it runs at DSL load in every binary, so a wrong rejection is a boot
// failure on every node in the cluster rather than one bad answer.
//
// A body with a statement before its return runs on the LogicRunner, whose
// scope resolves a bare DECLARED argument (memql#2364) -- so a conditional on
// `role == "owner"` there is not a constant, and the load must not refuse it.
// Nothing in this repo's tree writes that shape, so the boot canary cannot
// catch a regression; a product DSL bundle mounted at MEMQL_DSL_PATH goes
// through the same loader.
//
// The pre-2026 load also refused the complement -- a bare name that is
// neither a local nor a declared argument. An edition-2026 body refuses it
// when it runs (unknown_name) instead; that is not pinned here.
func TestLogicCondBareIdentifier_MultiStepDeclaredArgIsNotRejected(t *testing.T) {
	multiStep := strings.Join([]string{
		"@actor",
		"@description(\"multi-step declared-arg probe\")",
		"logic condMultiStepArgProbe {",
		"  args {",
		"    role string @required",
		"  }",
		"  body {",
		"    seen := actor.role ?? \"\"",
		"    return role == \"owner\" ? \"ALLOW\" : \"DENY\"",
		"  }",
		"}",
	}, "\n")

	_, err := tryParseNewFunctionSyntax(
		"condMultiStepArgProbe", "logic", multiStep, "common.logic.memql", dotAccessLoadRegistry())
	require.NoErrorf(t, err,
		"a MULTI-STEP body comparing a bare DECLARED ARG must load. That path runs on "+
			"the LogicRunner, whose scope resolves declared args (memql#2364), so the predicate "+
			"discriminates. Refusing it is a false positive on a load-path rule, which is a boot "+
			"failure on every node: %v", err)
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
