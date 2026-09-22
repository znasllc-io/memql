package datasync

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// operator_actor_test.go -- the runtime's own identity can WRITE (memql#5574).
//
// It could not. OperatorContext stamped the AccessContext and nothing else, so
// every read this package makes worked and every write answered `no actor found
// in context` -- `createdBy` resolves through TokenInfoFromContext, which is a
// different surface (memql#2989). Reads and writes are exercised by different
// tests, which is why a package this central was green over it.
//
// The assertions here are the four surfaces a write actually depends on, rather
// than "the helper returns a context". A test that only checked the
// AccessContext would have passed on the broken version.

func TestTheSyncOperatorCanActuallyWrite(t *testing.T) {
	ctx := OperatorContext(context.Background())

	// THE SURFACE THAT WAS MISSING, first. `mutationActor` reads this, and an
	// empty answer is `no actor found in context` on every mutation this
	// package makes -- WriteSyncState, SetPaused, and the outbox drain's own
	// status writes.
	if got := auth.ActorFromContext(ctx); got != syncOperatorActor {
		t.Errorf("ActorFromContext = %q, want %q -- this is what a `createdBy` stamp reads, and "+
			"an empty answer means the runtime can read its own bookkeeping and never write it",
			got, syncOperatorActor)
	}

	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext -- `actor.userId` in a filter resolves to nobody")
	}
	if !ac.IsClusterOwner() {
		t.Errorf("the sync operator is not a cluster owner (role %q); v1:platform:syncState and "+
			"outboxEntry are clusterOwner-tier, so this is zero rows and no error", ac.Role)
	}
	if !ac.Unranked {
		t.Error("the sync operator is ranked -- RoleOwner is not a claim to a rung, and a " +
			"rank-strict concept would read this as an owner acting on a PEER owner's row")
	}
	if !ac.Synthetic {
		t.Error("the sync operator is not marked synthetic -- the cluster can never own a row")
	}

	// INTERNAL ORIGIN is the separate axis this package also needs: several of
	// its mutations are @serverOnly, and auth.CallOrigin's zero value is
	// OriginClient, so a context that does not say otherwise is a client call
	// whatever actor it carries.
	if !auth.OriginFromContext(ctx).IsInternal() {
		t.Error("the sync operator's context is not internal-origin, so every @serverOnly " +
			"mutation this package calls is refused as a client call")
	}
}

// The two literals describing one actor, held together. A shared value with a
// restated copy beside it drifts silently: every read keeps working and the
// `createdBy` on new rows stops matching the one on old rows.
func TestSyncOperatorActorMatchesTheSharedHelper(t *testing.T) {
	ac := auth.SystemActor(syncOperatorActorName)
	if ac == nil {
		t.Fatalf("auth.SystemActor(%q) returned nil", syncOperatorActorName)
	}
	if ac.UserId != syncOperatorActor {
		t.Fatalf("auth.SystemActor(%q).UserId = %q, want %q -- the two literals have drifted",
			syncOperatorActorName, ac.UserId, syncOperatorActor)
	}
}
