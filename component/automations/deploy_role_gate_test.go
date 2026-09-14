package automations

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
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
// These run the SHIPPED gate bodies verbatim through RunLogicBody, compiled as
// the function loader compiles them, with a real AccessContext, because that
// is the only level at which the bug is visible.
// A gate that denies everyone is not a safe failure: the deploy-forward and
// rollback paths it guards become unreachable, and anyone debugging that
// reasonably concludes the role plumbing is broken rather than the resolver.

func runShippedGate(t *testing.T, name, body string, role auth.Role) any {
	t.Helper()
	src := "@actor\n@description(\"shipped gate under test\")\nlogic " + name + " {\n" + body + "\n}\n"
	_, steps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &bundleStepRegistry{}, nil)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: role})
	out, err := r.RunLogicBody(ctx, name, steps, map[string]any{})
	if err != nil {
		t.Fatalf("%s(role=%s): RunLogicBody: %v", name, role, err)
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
	src := "@actor\n@description(\"no-actor probe\")\nlogic noActorProbe {\n  role := actor.role ?? \"\"\n  return role\n}\n"
	_, steps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &bundleStepRegistry{}, nil)
	out, err := r.RunLogicBody(context.Background(), "noActorProbe", steps, map[string]any{})
	if err != nil {
		t.Fatalf("RunLogicBody: %v", err)
	}
	if out != "" {
		t.Errorf("role with no actor = %#v, want \"\"; anything non-empty is truthy and fails OPEN in a gate", out)
	}

	// And the gate built on it still denies.
	if got := runShippedGate(t, "deploymentForwardAllowed", forwardGateBody, ""); got != false {
		t.Errorf("forward gate with an empty role = %#v, want false", got)
	}
}

// A statement does NOT shadow an ambient root. `actor` is the one root
// security gates are written against, so it is the worst name for a
// statement to capture. The string evaluator once let a step named `actor`
// hijack the root at some sites and not others -- `actor.first().id` returned
// the leftover accessor text "().id", a TRUTHY string. A statement body
// cannot get there: a statement named after a reserved root is refused at
// load (body_reserved_name), so every `actor` a body reads is the actor.
func TestLogicStepDoesNotShadowAmbientRoot(t *testing.T) {
	src := `@description("statement named after an ambient root")
logic stepShadowsActor {
  args {
    id string @required
  }
  actor := query queryThing(id: args.id)
  picked := actor.first().id ?? "FALLBACK"
  return picked
}
`
	norm, err := languageParser.NormaliseAll(src)
	if err != nil {
		t.Fatal(err)
	}
	file, err := languageParser.ParseFile(norm)
	if err != nil {
		t.Fatal(err)
	}
	fn := file.Definitions[0].(*languageParser.FunctionDef)
	_, problems := compiler.CompileBody("logic", fn.Name, []string{"id"}, fn.Body.(*languageParser.AutomationDef).Body)
	var reserved bool
	for _, p := range problems {
		reserved = reserved || p.Code == "body_reserved_name"
	}
	if !reserved {
		t.Fatalf("a statement named `actor` must be refused at load (body_reserved_name), got %v", problems)
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
