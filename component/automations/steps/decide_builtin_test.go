package steps

// P1 (#2368) / S9 (#2407) pin tests: the decide-logic step shapes that only
// the DB-gated conformance suite exercised end-to-end. DB-free.

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// A cond() whose predicate is a COMPARISON routes through the real condition
// evaluator -- previously it evaluated as a non-empty literal string (always
// truthy) and silently picked the then-branch.
func TestEvaluateCond_ComparisonPredicate(t *testing.T) {
	ev := automations.NewEvaluator()
	ev.SetStepResult("role", &automations.StepResult{StepId: "role", Status: "success", Result: "owner"})

	got, err := sharedArgEvaluator.evaluateValue(ev, `cond(role == "owner", "queued", "needs_validation")`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "queued" {
		t.Fatalf("owner predicate must pick then-branch, got %v", got)
	}
	got, err = sharedArgEvaluator.evaluateValue(ev, `cond(role == "reader", "queued", "needs_validation")`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "needs_validation" {
		t.Fatalf("non-matching predicate must pick else-branch, got %v", got)
	}
	// Nested cond in the else arm -- the forge/deploypack decision-table shape.
	got, err = sharedArgEvaluator.evaluateValue(ev, `cond(role == "admin", true, cond(role == "owner", true, false))`)
	if err != nil {
		t.Fatal(err)
	}
	if got != true && got != "true" {
		t.Fatalf("nested cond else-arm must evaluate truthy, got %#v", got)
	}
}

// joinRawPositionalArgs round-trips positional builtin args VERBATIM in
// numeric order -- no re-quoting, no pre-evaluation.
func TestJoinRawPositionalArgs(t *testing.T) {
	got := joinRawPositionalArgs(map[string]any{
		"0": `role == "owner"`,
		"1": `"queued"`,
		"2": `cond(isPrivileged == true, "a", "b")`,
	})
	want := `role == "owner", "queued", cond(isPrivileged == true, "a", "b")`
	if got != want {
		t.Fatalf("raw positional join = %q, want %q", got, want)
	}
}
