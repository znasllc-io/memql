package automations

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// memql#2801: an UNBOUND actor root is fail-open, so every evaluator
// that can reach an `actor.*` read must bind the denying envelope.
//
// The evaluator renders an unresolved dotted path as its own path TEXT,
// so with `actor` unbound `actor.isClusterOwner` evaluates to the
// non-empty string "actor.isClusterOwner" -- truthy. A negated gate
// (`actor.isClusterOwner != false`) therefore read TRUE on a request
// with no auth context, which is the same fail-open the envelope's nil
// default was fixed for.
//
// This is the coverage the first attempt at that fix shipped without:
// reverting the binding left the whole suite green.
func TestBindActorEnvelope_UnboundActorIsFailOpen(t *testing.T) {
	// Baseline: what an UNBOUND actor root does. This is not asserting
	// desired behaviour -- it documents why binding is mandatory.
	bare := NewEvaluator()
	got, err := bare.EvaluateCondition("actor.isClusterOwner != false")
	if err != nil {
		t.Fatalf("unbound evaluate: %v", err)
	}
	if !got {
		t.Skip("unbound path no longer renders as literal text; the fail-open premise changed -- revisit memql#2801")
	}

	// Bound: the denying envelope closes it.
	bound := NewEvaluator()
	bindActorEnvelope(bound, context.Background())
	got, err = bound.EvaluateCondition("actor.isClusterOwner != false")
	if err != nil {
		t.Fatalf("bound evaluate: %v", err)
	}
	if got {
		t.Error("`actor.isClusterOwner != false` is TRUE with no auth context -- the admin gate is fail-open (memql#2801)")
	}

	// The positive form must deny too.
	got, err = bound.EvaluateCondition("actor.isClusterOwner == true")
	if err != nil {
		t.Fatalf("bound evaluate ==: %v", err)
	}
	if got {
		t.Error("`actor.isClusterOwner == true` must be false with no auth context")
	}
}

// A real owner must still pass, or the fix is a denial-of-service on the
// admin surface rather than a gate.
func TestBindActorEnvelope_RealOwnerStillPasses(t *testing.T) {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "u1", Role: auth.RoleOwner,
	})
	ev := NewEvaluator()
	bindActorEnvelope(ev, ctx)

	got, err := ev.EvaluateCondition("actor.isClusterOwner == true")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !got {
		t.Error("a real cluster owner must pass the gate")
	}
}

// The no-caller spelling (event triggers, scheduler ticks) must deny
// identically -- it is the same envelope, stated explicitly.
func TestBindNoCallerActorEnvelope_Denies(t *testing.T) {
	ev := NewEvaluator()
	bindNoCallerActorEnvelope(ev)
	for _, cond := range []string{
		"actor.isClusterOwner != false",
		"actor.isClusterOwner == true",
	} {
		got, err := ev.EvaluateCondition(cond)
		if err != nil {
			t.Fatalf("%s: %v", cond, err)
		}
		if got {
			t.Errorf("%s must be false for a trigger with no caller (memql#2801)", cond)
		}
	}
}
