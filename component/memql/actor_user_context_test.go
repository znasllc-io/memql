package memql

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The regression guard for memql#2989.
//
// `stamp { ownerUserId: actor.userId }` resolves through resolveActorReference,
// which reads the ACCESS CONTEXT (auth.AccessFromContext) and never the claims.
// The synthetic-actor helper used by every server-side handler that writes on a
// user's behalf originally set claims + TokenInfo ONLY, so the stamp resolved to
// the inbound caller -- or to "" on a detached context, because
// ActorEnvelopeValue returns ("", true) for a nil AccessContext rather than an
// error, which meant the row was written and the call SUCCEEDED.
//
// This lives in component/memql rather than component/auth on purpose: the bug
// was never visible from either side alone. auth's own tests would have shown
// three keys being set, and the DSL analyzer that gates owner tiers
// (rowauthz_owner_provenance.go) reasons over the loaded template and
// structurally cannot see which context the Go call sites build. Only the join
// of the two -- this test -- fails when they disagree.
func TestActorUserIdResolvesToTheStampedUserNotTheInboundCaller(t *testing.T) {
	inbound := auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "inbound-caller", Role: auth.RoleWriter})

	t.Run("inbound caller differs from the owner", func(t *testing.T) {
		// The case with teeth: a handler editing a document owned by someone
		// other than the caller. Resolving to the caller here silently
		// reassigns the row.
		got, err := resolveActorReference(auth.ContextWithUserActor(inbound, "real-owner"), "actor.userId")
		if err != nil {
			t.Fatalf("resolveActorReference: %v", err)
		}
		if got != "real-owner" {
			t.Errorf("actor.userId = %q, want %q -- a stamp under this context would attribute "+
				"the row to the wrong user", got, "real-owner")
		}
	})

	t.Run("detached context with no inbound access", func(t *testing.T) {
		// The async promotion goroutines. An empty owner here is worse than an
		// error: @rowAuthz(owner=...) gates reads on ownerUserId == actor.userId,
		// so the row becomes invisible to the user it was written for, with no
		// failure anywhere.
		got, err := resolveActorReference(auth.ContextWithUserActor(context.Background(), "real-owner"), "actor.userId")
		if err != nil {
			t.Fatalf("resolveActorReference: %v", err)
		}
		if got != "real-owner" {
			t.Errorf("actor.userId = %q, want %q", got, "real-owner")
		}
	})

	t.Run("a blank owner is a no-op, so call sites must refuse first", func(t *testing.T) {
		// Deliberately NOT a fallback. The helper cannot invent an owner, and
		// silently keeping the inbound one is the least surprising behaviour --
		// but it means every caller has to refuse before calling. Pinned so the
		// guards at those call sites are never read as belt-and-braces.
		got, _ := resolveActorReference(auth.ContextWithUserActor(inbound, ""), "actor.userId")
		if got != "inbound-caller" {
			t.Errorf("actor.userId = %q, want the inbound caller unchanged -- ContextWithUserActor "+
				"must not partially apply a blank owner", got)
		}
	})
}

// The helper sets all three surfaces, not just the two the original copies did.
// Separate from the test above because these are read by different consumers:
// createdBy and the mutation-actor presence check read the token surface, while
// actor.* reads the access surface. Losing either one is a different bug.
func TestContextWithUserActorSetsTokenAndAccessSurfaces(t *testing.T) {
	ctx := auth.ContextWithUserActor(context.Background(), "u-1")

	if actor := auth.ActorFromContext(ctx); actor == "" {
		t.Error("no actor on the token surface -- createdBy and the mutation-actor check read this")
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext -- actor.userId in a DSL stamp/filter reads this one")
	}
	if ac.UserId != "u-1" {
		t.Errorf("AccessContext.UserId = %q, want %q", ac.UserId, "u-1")
	}
}
