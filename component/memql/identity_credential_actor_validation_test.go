package memql

// Tests for the identity credential-row actor-scope guard (memql#2513):
// a user actor must not be able to forge a machine-credential
// v1:identity:identity row (badge / worker_token / node_token /
// voice_agent_token / service_account) over the raw mutation surface.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func userActorCtx(subject, role string) (context.Context, string) {
	tok := &auth.TokenInfo{
		Subject: subject,
		Claims:  map[string]any{"sub": subject, "role": role},
	}
	return auth.ContextWithToken(context.Background(), tok), subject
}

func systemActorCtx() (context.Context, string) {
	tok := &auth.TokenInfo{
		Subject: "system:planner",
		Claims:  map[string]any{"sub": "system:planner", "role": "system"},
	}
	return auth.ContextWithToken(context.Background(), tok), "system:planner"
}

func TestValidateIdentityCredentialActorScope(t *testing.T) {
	e := &MemQLEngine{}

	machineTypes := []string{"badge", "worker_token", "node_token", "voice_agent_token", "service_account"}

	// A user actor is rejected for every machine-credential type.
	for _, it := range machineTypes {
		t.Run("user_rejected_"+it, func(t *testing.T) {
			ctx, actor := userActorCtx("v1:identity:user:attacker", "writer")
			payload := map[string]any{
				"userId":       "v1:identity:user:victim",
				"identityType": it,
				"credentials":  map[string]any{"keyHash": "deadbeef"},
			}
			err := e.validateIdentityCredentialActorScope(ctx, payload, actor)
			if err == nil {
				t.Fatalf("user actor writing identityType=%q must be rejected", it)
			}
			if !strings.Contains(err.Error(), it) {
				t.Errorf("error should name the identityType; got %v", err)
			}
		})
	}

	// A system actor is allowed for every machine-credential type
	// (the legitimate mint handlers / CLI / bootstrap all run as one).
	for _, it := range machineTypes {
		t.Run("system_allowed_"+it, func(t *testing.T) {
			ctx, actor := systemActorCtx()
			payload := map[string]any{
				"userId":       "v1:identity:user:op",
				"identityType": it,
			}
			if err := e.validateIdentityCredentialActorScope(ctx, payload, actor); err != nil {
				t.Errorf("system actor writing identityType=%q must pass; got %v", it, err)
			}
		})
	}

	// Interactive / non-machine types are out of scope: a user actor
	// passes (their own write paths gate them).
	for _, it := range []string{"magic_link", "oauth", "api_key", ""} {
		t.Run("user_passes_noncredential_"+it, func(t *testing.T) {
			ctx, actor := userActorCtx("v1:identity:user:someone", "writer")
			payload := map[string]any{"userId": "v1:identity:user:someone"}
			if it != "" {
				payload["identityType"] = it
			}
			if err := e.validateIdentityCredentialActorScope(ctx, payload, actor); err != nil {
				t.Errorf("identityType=%q must not be gated by this guard; got %v", it, err)
			}
		})
	}

	// Nil payload is a no-op.
	if err := e.validateIdentityCredentialActorScope(context.Background(), nil, ""); err != nil {
		t.Errorf("nil payload must pass; got %v", err)
	}
}
