package automations

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// memql#2818: the shipped deploy role gates denied EVERY role, owner included.
//
// `role := actor.role ?? ""` inside a logic body resolved to the coalesce
// fallback rather than the actor's role, so every downstream `cond(role ==
// "owner", ...)` compared against "" and the gate returned false for everyone.
//
// The existing coverage could not see it: it exercises
// `EvaluateValue("$actor.role")`, which takes the $-form path. A logic STEP
// assignment takes a third path -- the runner's own coalesce-argument resolver
// -- and that one gated on a hardcoded root list omitting `actor`. So the
// gate's own spelling was the one shape nothing tested.
//
// These run the SHIPPED gate bodies verbatim through RunLogic with a real
// AccessContext, because that is the only level at which the bug is visible.
// A gate that denies everyone is not a safe failure: the deploy-forward and
// rollback paths it guards become unreachable, and anyone debugging that
// reasonably concludes the role plumbing is broken rather than the resolver.

func runShippedGate(t *testing.T, name, body string, role auth.Role) any {
	t.Helper()
	src := "@actor\n@description(\"shipped gate under test\")\nlogic " + name + " {\n  body {\n" + body + "\n  }\n}\n"
	parsed := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &bundleStepRegistry{}, nil)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: role})
	out, err := r.RunLogic(ctx, name, parsed, map[string]any{})
	if err != nil {
		t.Fatalf("%s(role=%s): RunLogic: %v", name, role, err)
	}
	return out
}

// The body is copied verbatim from dsl/deployment/logic.memql
// deploymentForwardAllowed.
const forwardGateBody = `    role := actor.role ?? ""
    isElevated := cond(role == "admin", true, cond(role == "owner", true, false))
    allowed := cond(role == "developer", true, isElevated)
    return allowed`

// And from deploymentRollbackAllowed -- owner-only, not even admin.
const rollbackGateBody = `    role := actor.role ?? ""
    allowed := cond(role == "owner", true, false)
    return allowed`

func TestDeploymentForwardAllowed_ResolvesActorRole(t *testing.T) {
	for _, tc := range []struct {
		role auth.Role
		want bool
	}{
		{auth.RoleOwner, true},
		{auth.RoleAdmin, true},
		{"developer", true},
		{auth.RoleWriter, false},
		{auth.RoleReader, false},
	} {
		got := runShippedGate(t, "deploymentForwardAllowed", forwardGateBody, tc.role)
		if got != tc.want {
			t.Errorf("deploymentForwardAllowed(role=%s) = %#v, want %v", tc.role, got, tc.want)
		}
	}
}

func TestDeploymentRollbackAllowed_ResolvesActorRole(t *testing.T) {
	for _, tc := range []struct {
		role auth.Role
		want bool
	}{
		{auth.RoleOwner, true},
		{auth.RoleAdmin, false}, // rollback is owner-only
		{auth.RoleWriter, false},
	} {
		got := runShippedGate(t, "deploymentRollbackAllowed", rollbackGateBody, tc.role)
		if got != tc.want {
			t.Errorf("deploymentRollbackAllowed(role=%s) = %#v, want %v", tc.role, got, tc.want)
		}
	}
}

// With no actor the gate must DENY -- and for the RIGHT reason.
//
// Asserting only "returns false" cannot tell the two worlds apart: before the
// fix `role` was the literal "actor.role" and every comparison failed; after,
// `role` is "". Both deny. So this pins the resolved VALUE, which is what
// distinguishes them -- and rules out the #2380 hazard shape, where an
// unresolved path becomes a non-empty and therefore truthy string.
func TestDeployGates_NoActorDeniesWithEmptyRole(t *testing.T) {
	src := "@actor\n@description(\"no-actor probe\")\nlogic noActorProbe {\n  body {\n    role := actor.role ?? \"\"\n    return role\n  }\n}\n"
	parsed := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &bundleStepRegistry{}, nil)
	out, err := r.RunLogic(context.Background(), "noActorProbe", parsed, map[string]any{})
	if err != nil {
		t.Fatalf("RunLogic: %v", err)
	}
	if out != "" {
		t.Errorf("role with no actor = %#v, want \"\"; anything non-empty is truthy and fails OPEN in a gate", out)
	}

	// And the gate built on it still denies.
	if got := runShippedGate(t, "deploymentForwardAllowed", forwardGateBody, ""); got != false {
		t.Errorf("forward gate with an empty role = %#v, want false", got)
	}
}

// A step may shadow an ambient root, and the step must win.
//
// isCustomVarRoot was widened to accept any seeded root, which made `actor` a
// resolvable root everywhere it is checked. At the two sites that had no step
// guard, that hijacked a step named `actor`: the $-form path cannot see step
// results, so `actor.first().id` returned the leftover accessor text "().id"
// -- not nil, not an error, a TRUTHY string that flows onward. Nothing in the
// shipped tree names a step `actor`, but `actor` is the one root security
// gates are written against, so it is the worst name to leave exposed.
func TestLogicStepShadowsAmbientRoot(t *testing.T) {
	src := `@description("step named after an ambient root")
logic stepShadowsActor {
  args {
    id string @required
  }
  body {
    actor := queryThing( id: args.id )
    picked := actor.first().id ?? "FALLBACK"
    return picked
  }
}
`
	parsed := parseLogicBody(t, src)
	reg := &bundleStepRegistry{nodes: []any{
		map[string]any{"id": "r1"},
		map[string]any{"id": "r2"},
	}}
	r := NewLogicRunner(&memql.MemQLEngine{}, reg, nil)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: auth.RoleOwner})
	out, err := r.RunLogic(ctx, "stepShadowsActor", parsed, map[string]any{"id": "x"})
	if err != nil {
		t.Fatalf("RunLogic: %v", err)
	}
	if out != "r1" {
		t.Errorf("step-shadowed `actor` resolved to %#v, want \"r1\" -- the step must win over the ambient root, and leftover accessor text like \"().id\" is truthy", out)
	}
}

// The equivalent coalesce() spelling must resolve identically. #2766 migrated
// the corpus to `??`, and the two forms must not diverge -- a resolver that
// understands only one of them is how a migration silently changes behaviour.
func TestDeployGate_CoalesceAndShorthandAgree(t *testing.T) {
	shorthand := runShippedGate(t, "gateShorthand", forwardGateBody, auth.RoleOwner)
	longhandBody := `    role := coalesce(actor.role, "")
    isElevated := cond(role == "admin", true, cond(role == "owner", true, false))
    allowed := cond(role == "developer", true, isElevated)
    return allowed`
	longhand := runShippedGate(t, "gateLonghand", longhandBody, auth.RoleOwner)
	if shorthand != longhand {
		t.Errorf("`??` gave %#v but coalesce() gave %#v; the two spellings must agree", shorthand, longhand)
	}
	if shorthand != true {
		t.Errorf("owner must pass the forward gate; got %#v", shorthand)
	}
}

// The gate bodies above are copied from dsl/deployment/logic.memql. That copy
// is exactly the "correct in two places" shape this fix was about, so pin it:
// if the shipped gate changes, these tests must be updated rather than
// silently continuing to pass against a stale duplicate.
func TestDeployGateBodiesMatchShippedDSL(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dsl", "deployment", "logic.memql"))
	if err != nil {
		t.Fatalf("read shipped logic.memql: %v", err)
	}
	src := string(raw)
	for _, tc := range []struct{ name, body string }{
		{"deploymentForwardAllowed", forwardGateBody},
		{"deploymentRollbackAllowed", rollbackGateBody},
	} {
		for _, line := range strings.Split(tc.body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if !strings.Contains(src, line) {
				t.Errorf("%s: dsl/deployment/logic.memql no longer contains %q; the copy in this file has gone stale -- update it so these tests keep testing the shipped gate", tc.name, line)
			}
		}
	}
}
