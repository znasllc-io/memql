package memql

import (
	"context"
	"encoding/json"
	"strings"
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
		"trace-bare": {
			// The BARE form must give the same reason as the dotted one. It
			// reaches unboundBareComparison first, whose diagnostic says "bind it
			// in a step first" -- impossible advice for a reserved name, which is
			// why that check defers a reserved-unsupplied root to this one.
			pred:  `trace == "x"`,
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

// TestLogicCondBareIdentifier_MultiStepDeclaredArgIsNotRejected is the
// false-positive guard, and it is the direction that matters most on this rule:
// it runs at DSL load in every binary, so a wrong rejection is a boot failure
// on every node in the cluster rather than one bad answer.
//
// A MULTI-STEP body dispatches to LogicRunner.RunLogic, whose bare-identifier
// tier resolves loop var -> step result -> DECLARED ARG (memql#2364). So
// `cond(<declaredArg> == ...)` there is not a constant: driven through RunLogic
// it returns ALLOW for role="owner" and DENY for role="reader". It also loaded
// on origin/main. The first cut of memql#3024 refused it anyway, on a rule that
// only knew about step ids -- and told the author their working gate was
// silently constant.
//
// Nothing in this repo's tree writes that shape (all 26 shipped cond predicates
// compare step-bound locals), so the boot canary could not catch it. A product
// DSL bundle mounted at MEMQL_DSL_PATH goes through the same loader, and
// examples/deploypack is one args-name choice away from it.
//
// The single-statement case in the sibling file must STAY rejected -- that is
// memql#3024's whole point -- so this test's value is that it pins both
// directions of a distinction the rule has to get exactly right.
func TestLogicCondBareIdentifier_MultiStepDeclaredArgIsNotRejected(t *testing.T) {
	multiStep := strings.Join([]string{
		"@enabled",
		"@actor",
		"@description(\"multi-step declared-arg probe\")",
		"logic condMultiStepArgProbe {",
		"  args {",
		"    role string @required",
		"  }",
		"  body {",
		"    seen := actor.role ?? \"\"",
		"    return cond(role == \"owner\", \"ALLOW\", \"DENY\")",
		"  }",
		"}",
	}, "\n")

	_, err := tryParseNewFunctionSyntax(
		"condMultiStepArgProbe", "logic", multiStep, "common.logic.memql", dotAccessLoadRegistry())
	require.NoErrorf(t, err,
		"a MULTI-STEP body comparing a bare DECLARED ARG must load. That path runs on "+
			"LogicRunner.RunLogic, whose bare-identifier tier resolves declared args "+
			"(memql#2364), so the predicate discriminates -- verified through RunLogic itself. "+
			"Refusing it is a false positive on a load-path rule, which is a boot failure on "+
			"every node, and the diagnostic asserts a constant where there is none: %v", err)

	// The complement: in the same multi-step shape, an identifier that is
	// NEITHER a step id NOR a declared arg still resolves against nothing and
	// must stay rejected. Without this the fix above would be a blanket
	// exemption for every multi-step body.
	undeclared := strings.Replace(multiStep, `role == "owner"`, `somethingNobodyDeclared == "x"`, 1)
	_, uErr := tryParseNewFunctionSyntax(
		"condMultiStepArgProbe", "logic", undeclared, "common.logic.memql", dotAccessLoadRegistry())
	require.Error(t, uErr,
		"a bare identifier that is neither a step id nor a declared arg resolves against "+
			"nothing on EITHER path, so it must still be refused in a multi-step body")
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

	// `partition` and `now` ride the same substitution as `config`, and until
	// now the only assertion on either was a LOAD-time check -- the same
	// zero-discriminating-power shape that let the `config` half ship
	// unverified. Both are single-segment roots, so they also exercise the
	// len(parts)==1 branch that the dotted paths never reach.
	//
	// The values are environment-dependent, so the assertion is on
	// DISCRIMINATION rather than on a literal: each predicate and its negation
	// must give opposite answers. A constant cannot do that.
	t.Run("single-segment-roots", func(t *testing.T) {
		for name, root := range map[string]string{"partition": "partition", "now": "now"} {
			t.Run(name, func(t *testing.T) {
				empty := run(t, "amb"+name+"Empty", root+` == ""`, nil)
				nonEmpty := run(t, "amb"+name+"NonEmpty", root+` != ""`, nil)
				require.NotEqualf(t, empty, nonEmpty,
					"`%s == \"\"` and `%s != \"\"` both returned %#v, so %q is not resolved during "+
						"expansion and any gate over it is a constant (memql#3024).",
					root, root, empty, root)
			})
		}
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
// the whole component/memql package stays green except for this test and
// TestExecute_CondAmbientConfigPredicate_Discriminates, whose baseCtx also
// carries no actor. Every OTHER test in the package -- including the `== true`
// sibling below that claims to cover this very guarantee -- passes with it
// removed.
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
