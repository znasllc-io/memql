package cognition

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestForwardedAuthClaimsForTurn_DailySpaceNoPlanId is the memql#1107
// regression. The ad-hoc produce path -- a deliverable running in a daily
// space with NO plan_id -- forwarded nil auth claims to the agent node, so
// the agent's tool-dispatch persistence (taskstamp.createAdHocPlan + the
// safety DecisionRecorder) ran on a ctx with no actor and failed with
// "no actor found in context". The plan_id path stamps fine because it
// forwards a system-actor claims map.
//
// The fix threads an actor onto the forwarded claims. This asserts both
// arms of forwardedAuthClaimsForTurn:
//
//  1. ctx carries the originating user's identity -> those claims ride
//     through, so the agent node reconstructs the real principal.
//  2. ctx carries NO identity (greet-on-join / cognition-initiated) ->
//     the cognition system-actor is forwarded so the map is never empty.
//
// In BOTH cases the claims, run through the same
// auth.ContextWithForwardedClaims the agent node uses to rebuild ctx, must
// yield a non-empty actor -- which is exactly what mutationActor reads
// before it errors with "no actor found in context".
func TestForwardedAuthClaimsForTurn_DailySpaceNoPlanId(t *testing.T) {
	t.Run("no identity on ctx falls back to system-actor", func(t *testing.T) {
		claims := forwardedAuthClaimsForTurn(context.Background())
		if len(claims) == 0 {
			t.Fatal("expected non-empty forwarded claims; nil claims reproduce the #1107 bug (no actor found in context on the ad-hoc produce path)")
		}
		assertForwardedActorNonEmpty(t, claims)
		if claims["sub"] != systemActorId {
			t.Fatalf("expected system-actor fallback sub=%q, got %q", systemActorId, claims["sub"])
		}
	})

	t.Run("user identity on ctx is forwarded", func(t *testing.T) {
		ctx := auth.ContextWithToken(context.Background(), &auth.TokenInfo{
			Subject: "v1:identity:user:user-123",
			Claims: map[string]any{
				"sub":   "v1:identity:user:user-123",
				"email": "user@example.com",
			},
		})
		claims := forwardedAuthClaimsForTurn(ctx)
		if len(claims) == 0 {
			t.Fatal("expected forwarded claims from the ctx identity, got empty")
		}
		assertForwardedActorNonEmpty(t, claims)
		if claims["sub"] != "v1:identity:user:user-123" {
			t.Fatalf("expected forwarded sub to be the originating user, got %q", claims["sub"])
		}
	})
}

// assertForwardedActorNonEmpty rebuilds the agent-node ctx exactly as the
// AiForwardRouter does (auth.ContextWithForwardedClaims) and asserts an
// actor resolves -- the precondition for taskstamp.createAdHocPlan and the
// safety recorder to persist instead of failing with "no actor found in
// context".
func assertForwardedActorNonEmpty(t *testing.T, claims map[string]string) {
	t.Helper()
	ctx := auth.ContextWithForwardedClaims(context.Background(), claims)
	if actor := auth.ActorFromContext(ctx); actor == "" {
		t.Fatal("rebuilt agent-node ctx has no actor; the ad-hoc produce path would fail with \"no actor found in context\"")
	}
}
