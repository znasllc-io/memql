package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/auth"
)

// cond_bare_identifier_predicate_3024_test.go -- memql#3024.
//
// The two residuals #2962 and its landing review left behind. Both are the
// same family: a cond PREDICATE that resolves to nothing and silently takes
// the else branch, so a gate written on it is open or closed by accident.
//
//  1. A BARE-IDENTIFIER predicate in a single-statement body:
//
//     body { return cond(role == "owner", "yes", "no") }
//
//     loads green, lints green, and returns "no" for EVERY input. In a
//     single-statement body there are no locals, so bare `role` resolves
//     against a nil scope and the comparison is constant. Bare `role` for an
//     arg is an authoring mistake -- args are read as `args.role` -- but the
//     failure mode is a wrong answer with no diagnostic, which is exactly what
//     #2962 exists to eliminate. It is now a LOAD ERROR naming the fix.
//
//  2. An AMBIENT predicate (`actor.` / `config.` / `partition` / `now` /
//     `trace`) was refused at load by validateLogicCondAmbientPredicate, which
//     #3024 replaces with real evaluation: the envelope is threaded through arg
//     expansion, so these predicates now discriminate like the `args.` ones.
//     That validator and its test are deleted in this change.
//
// # Why the bare-identifier rule has to be careful
//
// It sits on the load path for EVERY binary, so a false positive is a boot
// failure on every node -- the worst outcome available here. The rule must fire
// only when no local of that name is in scope, because the legitimate shape is
// live in the tree today:
//
//	role    := actor.role ?? ""                    // dsl/deployment/logic.memql
//	allowed := cond(role == "owner", true, false)
//
// TestLogicCondBareIdentifier_LiveTreeStillLoads is the canary for that: it
// loads the whole unified DSL tree, so a rule that rejects any shipped logic
// fails here rather than at boot.

// loadCondBarePredicateProbe loads a SINGLE-STATEMENT logic whose cond
// predicate is `pred`. Single-statement is the shape #3024 reports: no
// preceding step, therefore no local in scope.
func loadCondBarePredicateProbe(pred string) error {
	src := strings.Join([]string{
		"@enabled",
		"@actor",
		"@description(\"cond bare-identifier predicate probe\")",
		"logic condBarePredProbe {",
		"  args {",
		"    role string @required",
		"  }",
		"  body {",
		"    return cond(" + pred + ", \"elevated\", \"plain\")",
		"  }",
		"}",
	}, "\n")
	_, err := tryParseNewFunctionSyntax("condBarePredProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	return err
}

// TestLogicCondBareIdentifierPredicate_RejectedAtLoad is residual 1.
//
// Against the unfixed loader every one of these loads green and then returns
// the else branch for every input.
func TestLogicCondBareIdentifierPredicate_RejectedAtLoad(t *testing.T) {
	for name, pred := range map[string]string{
		"eq":        `role == "owner"`,
		"ne":        `role != "owner"`,
		"undeclared": `somethingNobodyDeclared == "x"`,
	} {
		t.Run(name, func(t *testing.T) {
			err := loadCondBarePredicateProbe(pred)
			require.Errorf(t, err,
				"cond(%s, ...) loaded green. In a single-statement body there is no local of "+
					"that name, so the bare identifier resolves against a nil scope and the "+
					"comparison is a CONSTANT -- the else branch for every input, silently. "+
					"That is memql#2962's mechanism in the spelling authors reach for first "+
					"(memql#3024).", pred)
			require.Containsf(t, err.Error(), "3024",
				"the rejection must cite the issue so the reason is findable, got: %v", err)
		})
	}

	// The diagnostic has to name the actual fix, or the author is told their
	// code is wrong without being told what to write. `args.` is the fix for
	// a declared arg, which is the overwhelmingly common case.
	err := loadCondBarePredicateProbe(`role == "owner"`)
	require.Error(t, err)
	require.Containsf(t, err.Error(), "args.role",
		"the error must name the corrected spelling `args.role`; a diagnostic that only says "+
			"'this is wrong' costs the author the same debugging session the silent constant did. got: %v", err)
}

// TestLogicCondBareIdentifierPredicate_LeavesLegitimateShapesAlone is the
// counterpart, and the one that matters most: this validator runs on every
// binary's load path.
func TestLogicCondBareIdentifierPredicate_LeavesLegitimateShapesAlone(t *testing.T) {
	// THE live shape. Both dsl/deployment/logic.memql and dsl/forge/logic.memql
	// bind the ambient value to a local and compare the local. Rejecting this
	// breaks boot on every node.
	localBound := strings.Join([]string{
		"@enabled",
		"@actor",
		"@description(\"local-bound probe\")",
		"logic condLocalBoundProbe {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    role := actor.role ?? \"\"",
		"    allowed := cond(role == \"owner\", true, false)",
		"    return allowed",
		"  }",
		"}",
	}, "\n")
	_, err := tryParseNewFunctionSyntax("condLocalBoundProbe", "logic", localBound, "common.logic.memql", dotAccessLoadRegistry())
	require.NoErrorf(t, err,
		"binding the ambient value to a local first is the CORRECT authoring and is what "+
			"dsl/deployment/logic.memql and dsl/forge/logic.memql already do. A rule that "+
			"cannot see the local in scope rejects it and breaks boot on every node: %v", err)

	// An args-rooted predicate is resolved by expansion (#2962) and must load.
	require.NoError(t, loadCondBarePredicateProbe(`args.role == "owner"`),
		"an args-rooted comparison predicate is resolved by expansion and must load")

	// The ambient roots are reserved top-level identifiers, never locals and
	// never payload fields. They are resolved by the envelope threading in this
	// same change, so they must LOAD (they were load errors before #3024).
	for name, pred := range map[string]string{
		"actor":     `actor.role == "owner"`,
		"config":    `config.someFlag == "on"`,
		"partition": `partition == "default"`,
	} {
		t.Run("ambient-"+name, func(t *testing.T) {
			require.NoErrorf(t, loadCondBarePredicateProbe(pred),
				"cond(%s, ...) must LOAD now: #3024 threads the ambient envelope through arg "+
					"expansion, so this evaluates instead of being refused. The "+
					"validateLogicCondAmbientPredicate load error it used to hit is deleted "+
					"in this change.", pred)
		})
	}
}

// TestLogicCondBareIdentifier_LiveTreeStillLoads is the boot canary.
//
// The bare-identifier rule runs on the load path for every binary. Loading the
// whole unified DSL tree is the only assertion that actually proves no shipped
// logic trips it -- reasoning about which shapes are live is exactly how a
// false positive reaches a node.
func TestLogicCondBareIdentifier_LiveTreeStillLoads(t *testing.T) {
	_, cErr := LoadUnifiedConcepts(nil)
	require.NoError(t, cErr, "LoadUnifiedConcepts")
	registry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, registry, memorynodes.DefaultRegistry())
	require.NoError(t, err,
		"the unified DSL tree must still load. The bare-identifier cond rule sits on the load "+
			"path for EVERY binary, so a false positive here is a boot failure on every node -- "+
			"the live local-bound gates in dsl/deployment/logic.memql and dsl/forge/logic.memql "+
			"are the shapes most likely to trip it (memql#3024).")
}

// condAmbientProbeSource builds a single-statement logic returning a cond over
// `pred`, for the evaluation tests below.
func condAmbientProbeSource(name, pred string) string {
	return strings.Join([]string{
		"@enabled",
		"@actor",
		"@description(\"memql#3024 ambient predicate probe\")",
		"logic " + name + " {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    return cond(" + pred + ", \"elevated\", \"plain\")",
		"  }",
		"}",
	}, "\n")
}

// TestExecute_CondAmbientPredicate_RunsAndDiscriminates is residual 2, and
// #3024's definition-of-done item 4 taken literally: driven through
// MemQLEngine.Execute rather than evalCollScalar.
//
// That distinction is not pedantry. #2962's first cut worked at the seam and
// failed end to end, because Execute substitutes args during expansion and then
// evaluates the plan root with NO args map at all. A seam test cannot see that.
//
// Postgres-gated, like its #2962 sibling.
func TestExecute_CondAmbientPredicate_RunsAndDiscriminates(t *testing.T) {
	eng, _, baseCtx := readMergeTestEngine(t)

	fn, err := tryParseNewFunctionSyntax(
		"ambientRoleGate", "logic",
		condAmbientProbeSource("ambientRoleGate", `actor.role == "owner"`),
		"memql#3024-test", memorynodes.DefaultRegistry())
	require.NoError(t, err,
		"an ambient cond predicate must LOAD -- #3024 replaces the load-time refusal with "+
			"evaluation")
	require.NoError(t, eng.Functions().Upsert(fn))

	call := func(role string) any {
		ctx := auth.ContextWithAccess(baseCtx, &auth.AccessContext{
			UserId: "u-" + role,
			Role:   auth.Role(role),
		})
		raw, mErr := json.Marshal("ignored")
		require.NoError(t, mErr)
		res, eErr := eng.Execute(ctx, "logic ambientRoleGate(a: "+string(raw)+")")
		require.NoErrorf(t, eErr, "ambientRoleGate must run to completion for role %q", role)
		require.NotNil(t, res)
		return res.OutputPayload()
	}

	owner, reader := call("owner"), call("reader")

	require.Equal(t, "elevated", owner, "an owner actor must take the then branch")
	require.Equal(t, "plain", reader, "a reader actor must take the else branch")

	// The load-bearing assertion. Both branches being reachable is what makes
	// this a gate rather than a constant.
	require.NotEqualf(t, owner, reader,
		"ambientRoleGate returned %#v for BOTH actors through Execute -- the predicate is not "+
			"evaluated against the resolved actor envelope, so the gate is open or closed by "+
			"accident rather than by the role (memql#3024).", owner)
}

// TestExecute_CondAmbientPredicate_AbsentActorDenies pins the fail-closed
// direction.
//
// buildAmbientEnvelope is built UNCONDITIONALLY (memql#2801): an absent auth
// context yields the DENYING envelope with every key present, rather than an
// empty map whose absent keys make a negated predicate evaluate true. A gate
// that opens when authentication is missing is worse than one that never fires.
func TestExecute_CondAmbientPredicate_AbsentActorDenies(t *testing.T) {
	eng, _, baseCtx := readMergeTestEngine(t)

	fn, err := tryParseNewFunctionSyntax(
		"ambientDenyGate", "logic",
		condAmbientProbeSource("ambientDenyGate", `actor.isClusterOwner == true`),
		"memql#3024-test", memorynodes.DefaultRegistry())
	require.NoError(t, err)
	require.NoError(t, eng.Functions().Upsert(fn))

	// baseCtx carries no AccessContext, so the envelope is the denying default.
	res, eErr := eng.Execute(baseCtx, `logic ambientDenyGate(a: "x")`)
	require.NoError(t, eErr)
	require.NotNil(t, res)
	require.Equal(t, "plain", res.OutputPayload(),
		"with no resolved actor the owner gate must DENY. An envelope that omits keys instead "+
			"of defaulting them is the memql#2801 fail-open: the predicate compares against a "+
			"missing value and a gate written this way opens for an unauthenticated caller.")
}
