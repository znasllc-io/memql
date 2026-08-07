package cognition

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// The memql#1107 regression, re-pinned against the mesh forwarded-auth
// contract (memql#3205).
//
// #1107: the ad-hoc produce path -- a deliverable running in a daily space
// with NO plan_id -- forwarded nil auth claims to the agent node, so the
// agent's tool-dispatch persistence (taskstamp.createAdHocPlan + the safety
// DecisionRecorder) ran on a ctx with no actor and failed with "no actor found
// in context". That property still has to hold, and it is asserted below
// through the same auth.ContextWithForwardedClaims the receiver uses.
//
// WHAT CHANGED, and why this test lost a case. The old builder had two arms:
// "prefer the originating user's identity on ctx" and "fall back to the
// cognition system-actor". The user arm was DEAD -- handleUtteranceForCognition
// roots its context at contextWithSystemActor(context.Background()) and nothing
// between that root and the forward reintroduces the inbound stream's
// principal, so it could not fire in production. The old test passed anyway by
// constructing a ctx by hand and feeding it straight to the builder, which
// tested the arm's arithmetic rather than its reachability.
//
// The builder is now unconditionally SYSTEM, and takes no ctx at all -- the
// shape that makes the dead arm unrepresentable rather than merely unused.
func TestForwardedPrincipalForTurnIsAlwaysASystemPrincipal(t *testing.T) {
	principal, err := forwardedPrincipalForTurn()
	if err != nil {
		t.Fatalf("building the cognition turn principal failed: %v", err)
	}

	if principal.Authority.Kind != auth.ForwardedPrincipalSystem {
		t.Errorf("principal kind = %v, want system", principal.Authority.Kind)
	}
	if principal.Authority.Subject != systemActorId {
		t.Errorf("subject = %q, want the cognition system actor %q", principal.Authority.Subject, systemActorId)
	}
	if principal.Authority.Role != auth.RoleReader {
		t.Errorf("role = %q, want reader.\n\n"+
			"Reader is the no-widening choice: this hop sent the invalid role string \"system\" "+
			"before, which IsValidRole rejects and FallbackFromClaims clamps to reader. RoleLevel "+
			"ranks writer(2) ABOVE reader(3) -- lower is more privileged -- so anything else here "+
			"GRANTS the mesh's system hops more than they have ever had.", principal.Authority.Role)
	}

	// The receiver must accept what the producer builds. If these two ever
	// disagree, every cognition -> agent forward fails closed.
	if _, err := auth.VerifyForwardedAuthority(principal.Authority, time.Now()); err != nil {
		t.Errorf("the receiver refused cognition's own principal: %v", err)
	}
}

// The #1107 property itself: whatever the producer ships must still rebuild
// into a non-empty actor on the agent node, or tool-dispatch persistence goes
// back to failing with "no actor found in context".
func TestForwardedPrincipalForTurnStillYieldsAnActorOnTheAgentNode(t *testing.T) {
	principal, err := forwardedPrincipalForTurn()
	if err != nil {
		t.Fatalf("building the cognition turn principal failed: %v", err)
	}

	ctx := auth.ContextWithForwardedClaims(context.Background(), principal.Claims)
	if actor := auth.ActorFromContext(ctx); actor == "" {
		t.Fatal("the rebuilt agent-node ctx has no actor; the ad-hoc produce path would fail " +
			"with \"no actor found in context\" -- memql#1107, all over again")
	}
}
