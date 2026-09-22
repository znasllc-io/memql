package edge

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// system_actor_shared_test.go -- the edge's synthetic cluster owner is the
// SHARED one, and its id string still reads exactly as before (memql#5574).
//
// Two literals describe one actor: `systemEdgeActor`, which tests and stored
// `createdBy` values assert verbatim, and `systemActorName`, which is what
// auth.SystemActor is handed. A shared value with a restated copy beside it
// drifts the moment somebody edits one of them, and the drift is silent --
// every read still works and the `createdBy` on new rows stops matching the
// one on old rows. So the two are held together here rather than by care.
func TestSystemEdgeActorMatchesTheSharedHelper(t *testing.T) {
	ac := auth.SystemActor(systemActorName)
	if ac == nil {
		t.Fatalf("auth.SystemActor(%q) returned nil -- the name is blank or the helper refused it",
			systemActorName)
	}
	if ac.UserId != systemEdgeActor {
		t.Fatalf("auth.SystemActor(%q).UserId = %q, want %q -- the two literals have drifted, "+
			"which changes the createdBy on every row this package writes from here on",
			systemActorName, ac.UserId, systemEdgeActor)
	}
}

// The properties this package's reads actually depend on. Asserted here and
// not only in component/auth, because a change there that looked harmless in
// its own tests is exactly what would take this package's reads dark: a
// non-owner Role fails the `actor.isClusterOwner == true` conjunct every one
// of these queries carries, and the result is zero rows and no error --
// which the resolver caches as "no such site".
func TestTheEdgeActorIsAClusterOwnerOnEveryReadableSurface(t *testing.T) {
	ctx := systemActorContext(context.Background())

	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext on the edge's own context -- `actor.userId` resolves to nobody")
	}
	if !ac.IsClusterOwner() {
		t.Errorf("the edge actor is not a cluster owner (role %q) -- every read in edge.go "+
			"carries `actor.isClusterOwner == true`, so this is zero rows and no error", ac.Role)
	}
	if !ac.Unranked {
		t.Error("the edge actor is ranked -- RoleOwner above is not a claim to a rung, and a " +
			"rank-strict concept would read this as an owner acting on a PEER owner's row")
	}
	if !ac.Synthetic {
		t.Error("the edge actor is not marked synthetic -- the cluster can never be a row's " +
			"owner, and a mutation stamping ownerUserId here would write a row nobody can read")
	}

	// TokenInfo is the surface memql#2989 is about and the one memql#5574
	// found missing elsewhere: `createdBy` resolves through it, not through
	// the AccessContext above.
	if got := auth.ActorFromContext(ctx); got != systemEdgeActor {
		t.Errorf("ActorFromContext = %q, want %q -- this is what a `createdBy` stamp reads, "+
			"and an empty answer is `no actor found in context` on every write", got, systemEdgeActor)
	}
}
