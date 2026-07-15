package identity

// Contract tests for ContextWithSystemCredentialActor (memql#2549): the
// machine-credential write paths must present an actor the memql#2513
// credential guard recognizes as a system actor. That guard's
// isSystemActor accepts an actor whose role is "system" OR whose derived
// actor string carries the "system:" prefix. The plain
// ContextWithSystemActor satisfies NEITHER (role="owner" and an email
// claim ActorFromToken prefers over the subject), which is the root cause
// of the /node/bootstrap loop; ContextWithSystemCredentialActor satisfies
// BOTH. These assertions pin that difference without a database.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestContextWithSystemCredentialActor_IsRecognizedAsSystem(t *testing.T) {
	ctx := ContextWithSystemCredentialActor(context.Background())

	actor := auth.ActorFromContext(ctx)
	if !strings.HasPrefix(actor, "system:") {
		t.Errorf("credential actor %q must carry the \"system:\" prefix so the memql#2513 guard's isSystemActor accepts it", actor)
	}

	id, err := auth.UserIdentityFromContext(ctx)
	if err != nil {
		t.Fatalf("UserIdentityFromContext: %v", err)
	}
	if !strings.EqualFold(id.Role, "system") {
		t.Errorf("credential actor role = %q, want \"system\" (the memql#2513 guard admits a machine-credential write only from role=system or a system:-prefixed actor)", id.Role)
	}
}

func TestContextWithSystemCredentialActor_OverridesExistingActor(t *testing.T) {
	// The /node/bootstrap + /pair/redeem HTTP surface already carries the
	// SystemActorMiddleware owner actor, and /admin/tokens carries the
	// logged-in operator; the credential helper must REPLACE that actor,
	// not defer to it (unlike the idempotent ContextWithSystemActor).
	base := ContextWithSystemActor(context.Background())
	if got := auth.ActorFromContext(base); strings.HasPrefix(got, "system:") {
		t.Fatalf("precondition: plain system actor %q unexpectedly carries the system: prefix", got)
	}

	overridden := ContextWithSystemCredentialActor(base)
	if got := auth.ActorFromContext(overridden); got != SystemCredentialActorSubject {
		t.Errorf("credential actor did not override the existing owner actor: got %q, want %q", got, SystemCredentialActorSubject)
	}
}

func TestContextWithSystemActor_NotRecognizedAsSystem(t *testing.T) {
	// Documents the root cause of memql#2549: the session-bootstrap actor
	// is deliberately NOT a system actor to the credential guard.
	ctx := ContextWithSystemActor(context.Background())

	actor := auth.ActorFromContext(ctx)
	if strings.HasPrefix(actor, "system:") {
		t.Errorf("session-bootstrap actor %q unexpectedly carries the system: prefix", actor)
	}

	id, err := auth.UserIdentityFromContext(ctx)
	if err != nil {
		t.Fatalf("UserIdentityFromContext: %v", err)
	}
	if strings.EqualFold(id.Role, "system") {
		t.Errorf("session-bootstrap actor role = %q, unexpectedly \"system\"", id.Role)
	}
}
