package automations

// #2380: the logic runner binds actor.* from the caller's auth context so
// role-gate logics (`role := actor.role ?? ""`) resolve at runtime.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestLogicRunnerEvaluator_BindsActor(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: auth.RoleOwner})
	ev := r.newEvaluatorForLogic(ctx, map[string]any{})
	got, err := evalV1(ev, "actor.role")
	if err != nil || got != "owner" {
		t.Fatalf("actor.role = %v err=%v, want owner", got, err)
	}
	// Absent auth: actor is BOUND to the denying envelope (memql#2801),
	// so reads resolve to the envelope's empty values -- not to nil, and
	// certainly not to the literal path text.
	ev = r.newEvaluatorForLogic(context.Background(), map[string]any{})
	got, _ = evalV1(ev, "actor.role")
	if got == "actor.role" {
		t.Fatal("unbound actor must not resolve to its own literal path")
	}
}

// memql#2801: the runner must bind the denying envelope on absent auth.
//
// Leaving actor unbound was fail-open: the string evaluator rendered an
// unresolved dotted path as its own path text, so `actor.isClusterOwner !=
// false` compared a non-empty string against false and read TRUE. Testing
// the binder helper in isolation does not catch a regression here --
// removing the runner's call to it leaves such a test green, which is how
// the first attempt at this fix shipped uncovered. RunScope now answers an
// unseeded actor with the same denying envelope, so the conditions below
// would stay false without the runner's binding: the SEEDING is asserted
// directly, so this seam's own regression still fails here.
func TestLogicRunnerEvaluator_AbsentAuthDeniesTheAdminGate(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	ev := r.newEvaluatorForLogic(context.Background(), map[string]any{})
	if _, seeded := ev.custom["actor"]; !seeded {
		t.Fatal("the logic runner did not seed `actor` on absent auth (memql#2801)")
	}

	for _, cond := range []string{
		"actor.isClusterOwner != false",
		"actor.isClusterOwner == true",
	} {
		got, err := evalV1Cond(t, ev, cond)
		if err != nil {
			t.Fatalf("%s: %v", cond, err)
		}
		if got {
			t.Errorf("%s is TRUE with no auth context -- the admin gate is fail-open (memql#2801)", cond)
		}
	}

	// A real owner must still pass, or this is a denial of service on the
	// admin surface rather than a gate.
	ownerCtx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: auth.RoleOwner})
	ev = r.newEvaluatorForLogic(ownerCtx, map[string]any{})
	got, err := evalV1Cond(t, ev, "actor.isClusterOwner == true")
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	if !got {
		t.Error("a real cluster owner must pass the gate")
	}
}
