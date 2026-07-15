package memql

// Real-engine regression guard for memql#2549 (the counterpart of the
// memql#2513 credential-actor guard). The /node/bootstrap persist path
// and its sibling machine-credential writes were rejected because they
// ran under the identity service's role="owner" session actor, which
// validateIdentityCredentialActorScope's isSystemActor does NOT accept --
// looping node_token bootstrap on every mesh node.
//
// This drives the REAL createNodeTokenIdentity / stampNodeTokenBootstrap /
// revokeNodeTokenIdentity mutations through Engine.Execute against a REAL
// Postgres with the guard + read-merge active (the readMergeTestEngine
// harness, Postgres-gated -- runs in CI's db-tests lane). It asserts the
// contract every fix site depends on: the role="owner" session-actor
// shape is rejected, the role="system" credential-actor shape is
// accepted, a genuine user actor is rejected, and the guard fires on the
// read-merged update() legs (stamp / revoke) too, not just the insert.
//
// The actor claim shapes below MIRROR component/identity/middleware.go:
// ownerSessionActorClaims == ContextWithSystemActor (the broken shape) and
// systemCredentialActorClaims == ContextWithSystemCredentialActor (the fix
// shape). component/memql cannot import component/identity (cycle), so the
// shapes are replicated here; if the identity helpers ever drift out of
// the guard contract, this suite catches it.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func actorCtx(claims map[string]any) context.Context {
	ctx := auth.ContextWithClaims(context.Background(), claims)
	return auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
}

// ownerSessionActorCtx mirrors identity.ContextWithSystemActor: role=owner
// plus an email claim ActorFromToken prefers over the system: subject.
func ownerSessionActorCtx() context.Context {
	return actorCtx(map[string]any{
		"sub":   "system:identity-svc",
		"email": "system@identity.memql.local",
		"role":  "owner",
	})
}

// systemCredentialActorCtx mirrors identity.ContextWithSystemCredentialActor:
// role=system and a system:-prefixed subject, no email.
func systemCredentialActorCtx() context.Context {
	return actorCtx(map[string]any{
		"sub":  "system:identity-credential",
		"role": "system",
	})
}

func writerUserActorCtx() context.Context {
	return actorCtx(map[string]any{
		"sub":   "v1:identity:user:attacker",
		"email": "attacker@example.com",
		"role":  "writer",
	})
}

// createNodeTokenCall builds the createNodeTokenIdentity mutation string
// exactly as component/identity/store.go's CreateNodeTokenIdentity does.
func createNodeTokenCall(identityId, nodeId string) string {
	return fmt.Sprintf(
		`mutation createNodeTokenIdentity(identityId: %q, userId: %q, nodeId: %q, nodeType: %q, keyHash: %q, mintedBy: %q, expiresAt: %q, bootstrappedAt: %q, bootstrappedFrom: %q)`,
		identityId, "system:node-bootstrap", nodeId, "bff", "hash-"+nodeId,
		"system:node-bootstrap", "", "", "")
}

func TestNodeTokenCredentialGuard_CreateActorScope(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)

	t.Run("owner_session_actor_rejected", func(t *testing.T) {
		id := "v1:identity:identity:node_test_owner_" + uniqueSuffix("2549")
		_, err := eng.Execute(ownerSessionActorCtx(), createNodeTokenCall(id, "n-owner"))
		if err == nil {
			t.Fatal("the role=owner session actor must be rejected by the memql#2513 credential guard (this is the mesh-wide node_token bootstrap loop, memql#2549)")
		}
		if !strings.Contains(err.Error(), "machine credential") {
			t.Errorf("expected the credential-actor guard error, got: %v", err)
		}
	})

	t.Run("system_credential_actor_accepted", func(t *testing.T) {
		id := "v1:identity:identity:node_test_cred_" + uniqueSuffix("2549")
		if _, err := eng.Execute(systemCredentialActorCtx(), createNodeTokenCall(id, "n-cred")); err != nil {
			t.Fatalf("the role=system credential actor must be allowed to write a node_token row (the memql#2549 fix): %v", err)
		}
	})

	t.Run("user_actor_rejected", func(t *testing.T) {
		id := "v1:identity:identity:node_test_user_" + uniqueSuffix("2549")
		_, err := eng.Execute(writerUserActorCtx(), createNodeTokenCall(id, "n-user"))
		if err == nil {
			t.Fatal("a user actor forging a node_token credential row must still be rejected (memql#2513 backstop)")
		}
	})
}

// TestNodeTokenCredentialGuard_ReadMergeUpdateLegs proves the guard fires
// on the update() legs too: identityType="node_token" is inherited from
// the persisted row via read-merge, so stamp (repeat bootstrap) and revoke
// (admin) are gated the same as the insert. The system credential actor
// passes both; a user actor is rejected on the revoke.
func TestNodeTokenCredentialGuard_ReadMergeUpdateLegs(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	sys := systemCredentialActorCtx()

	id := "v1:identity:identity:node_test_rm_" + uniqueSuffix("2549")
	if _, err := eng.Execute(sys, createNodeTokenCall(id, "n-rm")); err != nil {
		t.Fatalf("seed create under credential actor: %v", err)
	}

	// Repeat-bootstrap stamp leg (update; read-merge surfaces node_token).
	stamp := fmt.Sprintf(
		`mutation stampNodeTokenBootstrap(identityId: %q, keyHash: %q, bootstrappedAt: %q, bootstrappedFrom: %q)`,
		id, "hash-rotated", "2026-07-14T01:00:00Z", "5.6.7.8")
	if _, err := eng.Execute(sys, stamp); err != nil {
		t.Fatalf("stamp leg under credential actor must pass (read-merge node_token): %v", err)
	}

	// Admin revoke leg (update; read-merge surfaces node_token).
	revoke := fmt.Sprintf(`mutation revokeNodeTokenIdentity(identityId: %q)`, id)
	if _, err := eng.Execute(sys, revoke); err != nil {
		t.Fatalf("revoke under credential actor must pass: %v", err)
	}

	// The same read-merge revoke under a user actor is rejected, proving
	// the guard reads the merged identityType, not just insert literals.
	if _, err := eng.Execute(writerUserActorCtx(), revoke); err == nil {
		t.Fatal("a user-actor revoke of a node_token row must be rejected (read-merge surfaces identityType)")
	}
}
