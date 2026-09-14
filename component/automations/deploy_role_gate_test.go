package automations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// memql#2818: the shipped deploy role gates denied EVERY role, owner included.
//
// `role := actor.role ?? ""` inside a logic body resolved to the `??`
// fallback rather than the actor's role, so every downstream `role ==
// "owner"` compared against "" and the gate returned false for everyone. The
// string evaluator resolved a logic STEP assignment through its own
// coalesce-argument resolver, which gated on a hardcoded root list omitting
// `actor` -- so the gate's own spelling was the one shape nothing tested.
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
    isElevated := role == "admin" ? true : role == "owner" ? true : false
    allowed := role == "developer" ? true : isElevated
    return allowed`

// And from deploymentRollbackAllowed -- owner-only, not even admin.
const rollbackGateBody = `    role := actor.role ?? ""
    allowed := role == "owner" ? true : false
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

// A step does NOT shadow an ambient root: in edition 2026 a name that is a
// reserved root reads the root, never a step (args_resolution_v1.go; an
// automation naming a step after one is refused at load). `actor` is the one
// root security gates are written against, so it is the worst name for a
// step to capture. The string evaluator once let a step named `actor` hijack
// the root at some sites and not others -- `actor.first().id` returned the
// leftover accessor text "().id", a TRUTHY string.
//
// Here the read of `actor` is the actor envelope: `actor.first()` is not the
// step's rows, so `??` takes its fallback, and nothing read as a truthy
// string along the way.
func TestLogicStepDoesNotShadowAmbientRoot(t *testing.T) {
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
	if out != "FALLBACK" {
		t.Errorf("`actor.first().id ?? \"FALLBACK\"` = %#v, want the fallback: `actor` reads the actor root, never the step named after it", out)
	}
}

// The gate bodies above are copied from dsl/deployment/logic.memql. That copy
// is exactly the "correct in two places" shape this fix was about, so pin it:
// if the shipped gate changes, these tests must be updated rather than
// silently continuing to pass against a stale duplicate.
func TestDeployGateBodiesMatchShippedDSL(t *testing.T) {
	for _, e := range staleGateCopies(shippedDeployLogic(t)) {
		t.Error(e)
	}
	// The pin must be able to fail: a changed shipped gate is caught.
	tampered := strings.Replace(shippedDeployLogic(t), `allowed := role == "owner" ? true : false`, `allowed := role == "admin" ? true : false`, 1)
	if tampered == shippedDeployLogic(t) {
		t.Fatal("the shipped rollback gate is not spelled as expected; the tamper below changed nothing")
	}
	if errs := staleGateCopies(tampered); len(errs) == 0 {
		t.Error("a changed gate in the shipped file passed the pin")
	}
}

func shippedDeployLogic(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "dsl", "deployment", "logic.memql"))
	if err != nil {
		t.Fatalf("read shipped logic.memql: %v", err)
	}
	return string(raw)
}

// staleGateCopies returns one message per gate body this file carries that
// src does not contain.
func staleGateCopies(src string) []string {
	var out []string
	for _, tc := range []struct{ name, body string }{
		{"deploymentForwardAllowed", forwardGateBody},
		{"deploymentRollbackAllowed", rollbackGateBody},
	} {
		if missing := missingLines(src, tc.body); len(missing) != 0 {
			out = append(out, fmt.Sprintf("%s: dsl/deployment/logic.memql no longer contains %q; the copy in this file has gone stale -- update it so these tests keep testing the shipped gate", tc.name, missing))
		}
	}
	return out
}

// missingLines returns the non-blank lines of body that src does not contain.
func missingLines(src, body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.Contains(src, line) {
			out = append(out, line)
		}
	}
	return out
}
