package cognition

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// TestForwardedAuthorityForTurn_DailySpaceNoPlanId is the memql#1107
// regression, carried forward onto the mesh forwarded-auth contract
// (memql#3205).
//
// The original bug: the ad-hoc produce path -- a deliverable running in a
// daily space with NO plan_id -- forwarded nil auth to the agent node, so the
// agent's tool-dispatch persistence (taskstamp.createAdHocPlan + the safety
// DecisionRecorder) ran on a ctx with no actor and failed with "no actor found
// in context".
//
// What changed under the contract: the fix used to be "put some claims on the
// wire, including a role", and the receiver trusted them. Now the sender
// asserts a KIND and the receiver decides what that binds. The regression
// property is unchanged and still asserted below -- every arm must yield a
// non-empty actor on the rebuilt agent-node ctx -- but it is now reached
// without the wire ever naming a role.
func TestForwardedAuthorityForTurn_DailySpaceNoPlanId(t *testing.T) {
	t.Run("no principal on ctx falls back to the system actor", func(t *testing.T) {
		authority := forwardedAuthorityForTurn(context.Background())
		if authority == nil {
			t.Fatal("nil authority reproduces the #1107 bug (no actor found in context on the ad-hoc produce path)")
		}
		if authority.Kind != auth.AuthorityKindSystem {
			t.Fatalf("kind = %q, want system for a cognition-initiated turn", authority.Kind)
		}
		if authority.UserId != systemActorId {
			t.Fatalf("system actor = %q, want %q -- the audit trail distinguishes cognition-stamped rows from planner-stamped ones",
				authority.UserId, systemActorId)
		}
		assertForwardedActorNonEmpty(t, authority)
	})

	t.Run("an inbound forward is re-asserted verbatim", func(t *testing.T) {
		// The realistic cluster shape: BFF -> cognition -> agent. Hop two must
		// re-assert what hop one proved, expiry included -- rebuilding from
		// the AccessContext would keep the clamped role and lose the deadline.
		exp := time.Now().Add(time.Minute).Truncate(time.Second)
		inbound := &auth.ForwardedAuthority{
			Kind:         auth.AuthorityKindBadge,
			UserId:       "v1:identity:user:operator-9",
			PrimaryEmail: "operator@example.com",
			Role:         auth.RoleReader,
			BadgeExpires: exp,
		}
		ctx := auth.ContextWithForwardedAuthority(context.Background(), inbound)

		authority := forwardedAuthorityForTurn(ctx)
		if authority.Kind != auth.AuthorityKindBadge {
			t.Errorf("kind = %q, want badge; hop two downgraded the assertion", authority.Kind)
		}
		if !authority.BadgeExpires.Equal(exp) {
			t.Errorf("badge expiry lost on the cognition hop: got %v want %v; the agent node could not gate an expired grant",
				authority.BadgeExpires, exp)
		}
		if authority.Role != auth.RoleReader {
			t.Errorf("role = %q, want the clamped reader", authority.Role)
		}
		assertForwardedActorNonEmpty(t, authority)
	})

	t.Run("a co-resident actor on ctx is forwarded as a user", func(t *testing.T) {
		// Single-binary / co-resident origin: no inbound forward, but the
		// direct path resolved an actor whose role is already post-ceiling.
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId:       "v1:identity:user:user-123",
			PrimaryEmail: "user@example.com",
			Role:         auth.RoleWriter,
		})
		authority := forwardedAuthorityForTurn(ctx)
		if authority.Kind != auth.AuthorityKindUser {
			t.Fatalf("kind = %q, want user", authority.Kind)
		}
		if authority.UserId != "v1:identity:user:user-123" {
			t.Fatalf("forwarded subject = %q, want the originating user", authority.UserId)
		}
		assertForwardedActorNonEmpty(t, authority)
	})
}

// assertForwardedActorNonEmpty rebuilds the agent-node ctx exactly as
// HandleForwardedRequest does and asserts an actor resolves -- the
// precondition for taskstamp.createAdHocPlan and the safety recorder to
// persist instead of failing with "no actor found in context".
//
// It validates first, because under the contract an assertion that would be
// refused never reaches the binding step at all.
func assertForwardedActorNonEmpty(t *testing.T, authority *auth.ForwardedAuthority) {
	t.Helper()
	if err := authority.Validate(time.Now()); err != nil {
		t.Fatalf("the agent node would REFUSE this forward: %v", err)
	}
	ctx := auth.ContextWithForwardedAuthority(context.Background(), authority)
	if actor := auth.ActorFromContext(ctx); actor == "" {
		t.Fatal("rebuilt agent-node ctx has no actor; the ad-hoc produce path would fail with \"no actor found in context\"")
	}
}
