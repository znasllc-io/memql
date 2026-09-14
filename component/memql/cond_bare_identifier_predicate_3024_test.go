package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
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
//
// # In edition 2026
//
// cond() is retired (`p ? a : b`), and a logic body that returns an
// expression runs on the LogicRunner, which evaluates it with EvalExpr over
// the arguments, the statements' locals and the ambient envelope
// (logic_body_v1.go). Residual 1 has no edition-2026 path: the runner's scope
// resolves a bare DECLARED argument, as the pre-2026 multi-step tier did, and
// refuses any other unknown name as unknown_name when it runs -- a loud
// refusal, never a silent constant -- so the load-time rejection and its test
// went with the path that needed them. Residual 2 is what the tests below pin:
// an ambient predicate discriminates over the canonical envelope the runner
// binds (buildAmbientEnvelope), and the absent actor denies.

// loadCondBarePredicateProbe loads a SINGLE-STATEMENT logic whose cond
// predicate is `pred`. Single-statement is the shape #3024 reports: no
// preceding step, therefore no local in scope.
func loadCondBarePredicateProbe(pred string) error {
	src := strings.Join([]string{
		"@actor",
		"@description(\"cond bare-identifier predicate probe\")",
		"logic condBarePredProbe {",
		"  args {",
		"    role string @required",
		"  }",
		"  body {",
		"    return (" + pred + ") ? \"elevated\" : \"plain\"",
		"  }",
		"}",
	}, "\n")
	_, err := tryParseNewFunctionSyntax("condBarePredProbe", "logic", src, "common.logic.memql", dotAccessLoadRegistry())
	return err
}

// TestLogicCondBareIdentifierPredicate_LeavesLegitimateShapesAlone is the
// counterpart, and the one that matters most: this validator runs on every
// binary's load path.
func TestLogicCondBareIdentifierPredicate_LeavesLegitimateShapesAlone(t *testing.T) {
	// THE live shape. Both dsl/deployment/logic.memql and dsl/forge/logic.memql
	// bind the ambient value to a local and compare the local. Rejecting this
	// breaks boot on every node.
	localBound := strings.Join([]string{
		"@actor",
		"@description(\"local-bound probe\")",
		"logic condLocalBoundProbe {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    role := actor.role ?? \"\"",
		"    allowed := role == \"owner\" ? true : false",
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
	// `config.demoMode`, not a made-up key. An allow-listed key is the whole
	// point: a path the envelope does not carry folds to nil and is a silent
	// constant, so it is a LOAD ERROR now rather than a legitimate shape --
	// see TestLogicCondBareIdentifierPredicate_RejectsUnresolvableAmbientPaths.
	// The earlier `config.someFlag` here asserted only "did not error at load"
	// on a key that never resolves, so it would have passed identically with
	// config resolution completely broken.
	for name, pred := range map[string]string{
		"actor":     `actor.role == "owner"`,
		"config":    `config.demoMode == true`,
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
		"@actor",
		"@description(\"memql#3024 ambient predicate probe\")",
		"logic " + name + " {",
		"  args {",
		"    a string @required",
		"  }",
		"  body {",
		"    return (" + pred + ") ? \"elevated\" : \"plain\"",
		"  }",
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
	ret, ok := fn.Expr.(*PlanConstExpression)
	require.Truef(t, ok, "fn.Expr = %T, want the returned expression as a *PlanConstExpression", fn.Expr)
	scope := MapScope{"args": args}
	for k, v := range eng.AmbientEnvelope(ctx) {
		scope[k] = v
	}
	got, err := EvalExpr(ctx, ret.Expr, scope, EvalOptions{})
	require.NoErrorf(t, err, "evaluating %s", pred)
	return got
}

// TestCondAmbientPredicate_DiscriminatesOverTheEnvelope is residual 2: an
// ambient predicate is evaluated against the resolved actor, so an owner and a
// reader take different branches. (Before edition 2026 this was driven through
// MemQLEngine.Execute, because the pre-2026 path expanded the arguments and
// then evaluated the plan root with no arguments at all; an edition-2026 body
// runs on the LogicRunner, which component/automations provides and drives
// end to end in its logic corpus.)
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
