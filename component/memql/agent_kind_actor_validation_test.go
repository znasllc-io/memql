package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// userActorContext builds a context that carries a plain user-role
// TokenInfo. Mirrors what the frontend's agent-creation-modal call
// lands on the engine with: subject is the user id, role is "user" (NOT
// "system"), and the actor string the engine extracts from it does
// not begin with "system:".
func userActorContext() (context.Context, string) {
	token := &auth.TokenInfo{
		Subject: "user-abc",
		Claims: map[string]any{
			"sub":  "user-abc",
			"role": "user",
		},
	}
	return auth.ContextWithToken(context.Background(), token), "user-abc"
}

// systemPlannerContext mirrors integrations/planner/agent_loop.go's
// systemActorContext: subject "system:planner", role "system". This
// is the context the planner integration's createAgent call
// runs under when auto-provisioning a kind="specialist" agent.
func systemPlannerContext() (context.Context, string) {
	token := &auth.TokenInfo{
		Subject: "system:planner",
		Claims: map[string]any{
			"sub":  "system:planner",
			"role": "system",
		},
	}
	return auth.ContextWithToken(context.Background(), token), "system:planner"
}

// systemSeedContext mirrors component/memql/seed_materializer.go's
// systemActorContext. This is the context the SeedMaterializer writes
// kind="system" platform agents (MemQL Planner, MemQL Trainer) under.
func systemSeedContext() (context.Context, string) {
	token := &auth.TokenInfo{
		Subject: "system:seedMaterializer",
		Claims: map[string]any{
			"sub":  "system:seedMaterializer",
			"role": "system",
		},
	}
	return auth.ContextWithToken(context.Background(), token), "system:seedMaterializer"
}

// The validator does not touch any field on MemQLEngine -- it inspects
// payload, actor, and the context's identity only. Tests therefore
// invoke it through a nil receiver to keep them DB-free.
func validatorOnNilEngine() *MemQLEngine { return nil }

// TestAgentKindActorScope_UserActor_RejectsSystem pins the primary
// reject case: a user-actor call that passes kind="system" rejects
// with a typed error that names the allowed value ("assistant").
func TestAgentKindActorScope_UserActor_RejectsSystem(t *testing.T) {
	ctx, actor := userActorContext()
	payload := map[string]any{"kind": "system", "name": "Sneaky"}

	err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor)
	if err == nil {
		t.Fatal("expected rejection for user-actor kind=system; got nil")
	}
	for _, want := range []string{"system actor", `"assistant"`, "v1:agents:agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

// TestAgentKindActorScope_UserActor_RejectsSpecialist pins the
// matching reject for the other privileged kind. The CreateAgentModal
// must never be able to backdoor a specialist row.
func TestAgentKindActorScope_UserActor_RejectsSpecialist(t *testing.T) {
	ctx, actor := userActorContext()
	payload := map[string]any{"kind": "specialist", "name": "Ghost"}

	err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor)
	if err == nil {
		t.Fatal("expected rejection for user-actor kind=specialist; got nil")
	}
	if !strings.Contains(err.Error(), `"specialist"`) && !strings.Contains(err.Error(), "specialist") {
		t.Errorf("error should name the rejected kind; got %q", err.Error())
	}
}

// TestAgentKindActorScope_UserActor_AllowsAssistant pins the happy
// path for the SPA flow: an explicit kind="assistant" passes for a
// user actor.
func TestAgentKindActorScope_UserActor_AllowsAssistant(t *testing.T) {
	ctx, actor := userActorContext()
	payload := map[string]any{"kind": "assistant", "name": "Sofia"}

	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor); err != nil {
		t.Fatalf("expected user-actor kind=assistant to pass; got %v", err)
	}
}

// TestAgentKindActorScope_UserActor_AllowsOmittedKind pins the
// no-opinion path: a payload that doesn't carry the kind field at all
// (an update that doesn't touch kind, for example) passes without
// asking what the existing row's kind is.
func TestAgentKindActorScope_UserActor_AllowsOmittedKind(t *testing.T) {
	ctx, actor := userActorContext()
	payload := map[string]any{"name": "Sofia"}

	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor); err != nil {
		t.Fatalf("expected payload without kind to pass; got %v", err)
	}
}

// TestAgentKindActorScope_UserActor_AllowsEmptyKind pins the
// no-opinion path for empty kind: a payload that carries kind="" is
// equivalent to "use the mutation default" and shouldn't reject.
func TestAgentKindActorScope_UserActor_AllowsEmptyKind(t *testing.T) {
	ctx, actor := userActorContext()
	payload := map[string]any{"kind": "", "name": "Sofia"}

	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor); err != nil {
		t.Fatalf("expected user-actor kind='' to pass; got %v", err)
	}
}

// TestAgentKindActorScope_UserActor_IgnoresUnknownKind pins that
// values outside the enum are not this validator's concern. The
// concept schema's enum check is the authoritative source on
// malformed values; we don't want to double-error.
func TestAgentKindActorScope_UserActor_IgnoresUnknownKind(t *testing.T) {
	ctx, actor := userActorContext()
	payload := map[string]any{"kind": "wizard", "name": "Sofia"}

	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor); err != nil {
		t.Fatalf("expected unknown kind to pass through this validator; got %v", err)
	}
}

// TestAgentKindActorScope_SystemPlanner_AllowsSpecialist pins the
// planner integration's contract: factory.go:buildCreateAgentArgs
// stamps kind="specialist" under the system:planner actor and must
// not be blocked.
func TestAgentKindActorScope_SystemPlanner_AllowsSpecialist(t *testing.T) {
	ctx, actor := systemPlannerContext()
	payload := map[string]any{"kind": "specialist", "name": "AccountingAgent"}

	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor); err != nil {
		t.Fatalf("expected system:planner kind=specialist to pass; got %v", err)
	}
}

// TestAgentKindActorScope_SystemSeed_AllowsSystem pins the seed
// materializer's contract: it writes kind="system" platform agents
// (plannerAgent, trainerAgent) under the system:seedMaterializer
// actor.
func TestAgentKindActorScope_SystemSeed_AllowsSystem(t *testing.T) {
	ctx, actor := systemSeedContext()
	payload := map[string]any{"kind": "system", "name": "MemQL Planner"}

	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, actor); err != nil {
		t.Fatalf("expected system:seedMaterializer kind=system to pass; got %v", err)
	}
}

// TestAgentKindActorScope_SystemActorOnlyByRole pins the second of
// the two isSystemActor branches: a caller with role="system" whose
// actor string does NOT have the "system:" prefix still counts as a
// system actor. Covers any future backend role-elevated path that
// runs without an explicit system:<x> subject.
func TestAgentKindActorScope_SystemActorOnlyByRole(t *testing.T) {
	token := &auth.TokenInfo{
		Subject: "internal-backend-service",
		Claims: map[string]any{
			"sub":  "internal-backend-service",
			"role": "system",
		},
	}
	ctx := auth.ContextWithToken(context.Background(), token)
	payload := map[string]any{"kind": "system", "name": "Internal"}

	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, payload, "internal-backend-service"); err != nil {
		t.Fatalf("expected role=system caller without system:* actor to pass; got %v", err)
	}
}

// TestAgentKindActorScope_NilPayload is a defensive guard for the
// only nil path the wrapper can hand it. Returns nil silently.
func TestAgentKindActorScope_NilPayload(t *testing.T) {
	ctx, actor := userActorContext()
	if err := validatorOnNilEngine().validateAgentKindActorScope(ctx, nil, actor); err != nil {
		t.Fatalf("expected nil payload to pass; got %v", err)
	}
}
