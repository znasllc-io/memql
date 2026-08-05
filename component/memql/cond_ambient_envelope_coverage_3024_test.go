package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	busv1 "github.com/znasllc-io/memql/component/bus/gen"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// cond_ambient_envelope_coverage_3024_test.go closes the gaps the memql#3024
// landing review found in the first cut of that change.
//
// All four tests here answer the same question from different sides: WHICH
// ambient paths actually resolve, and does anything notice when one stops?
// The first cut answered it three different ways in three places -- isAmbientRoot
// accepted five roots, buildAmbientEnvelope supplied four, and the LogicRunner
// bound one -- so a predicate could be accepted by the validator, folded to nil
// by expansion, and silently take the else branch forever. That is memql#2962's
// defect reached through memql#3024's own fix.

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

// TestLogicCondBareIdentifierPredicate_RejectsUnresolvableAmbientPaths is the
// regression guard for the residual the landing review found.
//
// memql#3024 deleted validateLogicCondAmbientPredicate, which refused EVERY
// ambient cond predicate at load, on the premise that the envelope now resolves
// them. It resolves the ones it carries. For the rest -- `trace.` (reserved but
// supplied by nothing) and any leaf the envelope has no key for -- the deletion
// traded a loud boot error for a silent constant, which is strictly the worse
// failure and is the exact defect memql#3024 exists to eliminate.
//
// The `trace` and `config` cases below LOADED GREEN before this fix and
// returned the else branch for every input.
//
// The two `actor` cases were ALREADY covered, by the pre-existing closed-set
// check that rejects an unknown actor member (#2623) -- they are kept because
// this is the property that matters and it should be pinned wherever it is
// enforced from. If that check is ever relaxed, these fail here rather than
// becoming silent constants in production.
func TestLogicCondBareIdentifierPredicate_RejectsUnresolvableAmbientPaths(t *testing.T) {
	for name, tc := range map[string]struct{ pred, wants string }{
		"trace-root": {
			// `trace` is reserved by the parser and by dslfs.reservedAlias, so it
			// cannot be shadowed -- but nothing anywhere populates a trace object
			// into any evaluation envelope.
			pred:  `trace.id == "x"`,
			wants: "reserved name that no evaluation envelope supplies",
		},
		"config-not-allow-listed": {
			// Not in component/config/policy_exposable.go's allow-list.
			pred:  `config.someFlag == "on"`,
			wants: "does not carry",
		},
		"actor-unknown-leaf": {
			// CLAUDE.md's argument-resolution table lists actor.partitions, but
			// ActorEnvelopeMap does not emit it. Documented and unresolvable is
			// the worst combination: an author has every reason to write it.
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
				"cond(%s, ...) resolves to nothing on every evaluation path, so it is a CONSTANT "+
					"that takes the else branch for every input. It must be refused at load, as "+
					"validateLogicCondAmbientPredicate refused it before memql#3024 (memql#2962).",
				tc.pred)
			require.Contains(t, err.Error(), tc.wants)
		})
	}
}

// TestExecute_CondAmbientConfigPredicate_Discriminates is memql#3024's
// definition-of-done bullet 2 for the half that was never demonstrated: the
// bullet names `config.X` as explicitly as `actor.X`, and bullet 4 demands
// EACH be driven through MemQLEngine.Execute.
//
// The first cut's only config assertion was a load-time check on a key that
// does not exist, so it would have passed with config resolution entirely
// removed. This one cannot: it runs the same predicate against two engines
// whose snapshots differ and requires the answers to differ.
func TestExecute_CondAmbientConfigPredicate_Discriminates(t *testing.T) {
	run := func(t *testing.T, name, pred string, snapshot *busv1.ConfigSnapshot) any {
		t.Helper()
		eng, _, baseCtx := readMergeTestEngine(t)
		eng.SetConfigSnapshot(snapshot)

		fn, err := tryParseNewFunctionSyntax(
			name, "logic", condAmbientProbeSource(name, pred),
			"memql#3024-test", memorynodes.DefaultRegistry())
		require.NoError(t, err, "an allow-listed config predicate must LOAD")
		require.NoError(t, eng.Functions().Upsert(fn))

		raw, mErr := json.Marshal("ignored")
		require.NoError(t, mErr)
		res, eErr := eng.Execute(baseCtx, "logic "+name+"(a: "+string(raw)+")")
		require.NoError(t, eErr)
		require.NotNil(t, res)
		return res.OutputPayload()
	}

	t.Run("bool", func(t *testing.T) {
		on := run(t, "cfgDemoOn", `config.demoMode == true`, &busv1.ConfigSnapshot{DemoMode: true})
		off := run(t, "cfgDemoOff", `config.demoMode == true`, &busv1.ConfigSnapshot{DemoMode: false})
		require.Equal(t, "elevated", on)
		require.Equal(t, "plain", off)
		require.NotEqualf(t, on, off,
			"cond(config.demoMode == true, ...) returned %#v under BOTH snapshots, so the "+
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
}

// TestExecute_CondAmbientPredicate_NegatedAbsentActorDenies pins the memql#2801
// guarantee that engine.go's own comment claims -- and that its sibling test
// does not actually cover.
//
// The distinction is the whole point. `actor.isClusterOwner == true` denies on
// an absent actor whether the envelope is built or not, because an unresolved
// predicate also falls to the else branch: the expected value is identical
// under the fix and under the bug, so the assertion is blind. Comparing against
// the DENYING VALUE is what separates them. With the envelope built
// unconditionally, `isClusterOwner != false` is false and the gate denies; with
// the key absent, specEqual short-circuits on nil, `!=` reads TRUE, and an
// "if you ARE an owner" gate opens for an unauthenticated caller.
//
// Measured: with `ambient` made conditional on an AccessContext being present,
// the whole component/memql package stays green -- except for this test.
func TestExecute_CondAmbientPredicate_NegatedAbsentActorDenies(t *testing.T) {
	for name, pred := range map[string]string{
		"isClusterOwner": `actor.isClusterOwner != false`,
		"role":           `actor.role != ""`,
	} {
		t.Run(name, func(t *testing.T) {
			eng, _, baseCtx := readMergeTestEngine(t)

			fnName := "ambientNegatedDenyGate" + name
			fn, err := tryParseNewFunctionSyntax(
				fnName, "logic", condAmbientProbeSource(fnName, pred),
				"memql#3024-test", memorynodes.DefaultRegistry())
			require.NoError(t, err)
			require.NoError(t, eng.Functions().Upsert(fn))

			// baseCtx deliberately carries no AccessContext.
			_, hasAccess := auth.AccessFromContext(baseCtx)
			require.False(t, hasAccess, "this test is only meaningful with no resolved actor")

			res, eErr := eng.Execute(baseCtx, `logic `+fnName+`(a: "x")`)
			require.NoError(t, eErr)
			require.NotNil(t, res)
			require.Equalf(t, "plain", res.OutputPayload(),
				"cond(%s, ...) took the THEN branch with no authenticated caller. That is the "+
					"memql#2801 fail-open: the envelope must be built unconditionally so every key "+
					"is present with a denying value, because an ABSENT key makes a negated "+
					"predicate read true and opens the gate for an unauthenticated caller.", pred)
		})
	}
}
