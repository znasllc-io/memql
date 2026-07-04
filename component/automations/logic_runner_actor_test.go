package automations

// #2380: the logic runner binds actor.* from the caller's auth context so
// role-gate logics (`role := coalesce(actor.role, "")`) resolve at runtime.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestLogicRunnerEvaluator_BindsActor(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "u1", Role: auth.RoleOwner})
	ev := r.newEvaluatorForLogic(ctx, map[string]any{})
	got, err := ev.EvaluateValue("$actor.role")
	if err != nil || got != "owner" {
		t.Fatalf("actor.role = %v err=%v, want owner", got, err)
	}
	// Absent auth: actor stays unbound; reads resolve nil, never the literal.
	ev = r.newEvaluatorForLogic(context.Background(), map[string]any{})
	got, _ = ev.EvaluateValue("$actor.role")
	if got == "actor.role" {
		t.Fatal("unbound actor must not resolve to its own literal path")
	}
}
