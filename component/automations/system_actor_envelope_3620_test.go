package automations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// memql#3620. An automation reaches the engine through contextWithSystemActor,
// which used to stamp claims + TokenInfo and nothing else. `createdBy` and the
// engine's mutation-actor presence check read those two; `actor.userId` in a
// DSL `stamp { }` reads the AccessContext, which was absent -- so it rendered
// as the EMPTY STRING.
//
// That is not theoretical here. dsl/forge's routeRequest and recordTransition
// both persist through `mutate requestEvent recordRequestEvent`, whose insert
// stamps `actorUserId: actor.userId` on a REQUIRED field carrying
// `@relationship(target=user)`. Every routed request and every status
// transition therefore wrote an audit row naming nobody, and nothing noticed:
// `@required` means present, not non-empty.
//
// An automation IS a principal, and it already had a name -- the same
// `system:automation:<name>` this helper puts in the `sub` claim. Writing that
// name is honest; writing "" is not.

func TestSystemActorStampsABindableAccessContext(t *testing.T) {
	ctx := contextWithSystemActor(context.Background(), "routeRequest")

	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext -- actor.userId in an automation's mutation stamp reads " +
			"this one, and without it every such write lands an empty actor (memql#3620)")
	}
	want := systemActorPrefix + "routeRequest"
	if ac.UserId != want {
		t.Errorf("AccessContext.UserId = %q, want %q -- the same principal the `sub` claim "+
			"already names", ac.UserId, want)
	}
	if _, _, err := auth.ActorEnvelopeBind(ac, "userId"); err != nil {
		if errors.Is(err, auth.ErrActorEnvelopeNoCaller) {
			t.Errorf("the stamped actor must BIND, not merely exist -- a DSL stamp reading "+
				"actor.userId under this context is refused: %v", err)
		} else {
			t.Errorf("unexpected bind failure: %v", err)
		}
	}
}

// RoleReader, not owner. memql#2801 made the envelope guarantee
// isClusterOwner=false for an absent caller because `actor.isClusterOwner==true`
// is the ENTIRE gate on the admin-tier constructs; that guarantee must not
// weaken now that background automations have a caller.
func TestSystemActorIsNotAClusterOwner(t *testing.T) {
	ctx := contextWithSystemActor(context.Background(), "anything")
	ac, _ := auth.AccessFromContext(ctx)
	if ac.IsClusterOwner() {
		t.Fatal("the automation principal must never be a cluster owner -- that bit is the " +
			"whole admin-tier gate (memql#2801)")
	}
	owner, _ := auth.ActorEnvelopeValue(ac, "isClusterOwner")
	if owner != false {
		t.Errorf("actor.isClusterOwner = %v, want false", owner)
	}
	if !auth.IsValidRole(ac.Role) {
		t.Errorf("Role = %q is outside the closed enum, so every rank comparison reads a "+
			"role that ranks below nothing", ac.Role)
	}
}

// It fills in ONLY when absent. AuthoredScheduler runs an owner's automations
// under AuthorContext, which stamps the AUTHOR's AccessContext precisely so
// per-row authz confines the run to what the author may touch; overwriting it
// would hand every authored automation the engine's principal instead -- the
// exact opposite of that helper's purpose.
func TestSystemActorDoesNotClobberAnAuthorsAccessContext(t *testing.T) {
	authored := AuthorContext(context.Background(), "user-author-3620")
	ctx := contextWithSystemActor(authored, "someAuthoredAutomation")

	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac.UserId != "user-author-3620" {
		t.Fatalf("AccessContext = %+v, want the AUTHOR's; an authored automation must keep "+
			"running under its author's authz envelope", ac)
	}
}

// The claims/TokenInfo surfaces keep their existing meaning -- `createdBy` and
// the engine's "no actor found in context" check read those, and this change
// must not disturb either.
func TestSystemActorStillStampsTheTokenSurfaces(t *testing.T) {
	ctx := contextWithSystemActor(context.Background(), "cronSweep")

	actor := auth.ActorFromContext(ctx)
	if !strings.HasPrefix(actor, systemActorPrefix) {
		t.Errorf("token-surface actor = %q, want the %q-prefixed automation principal "+
			"(createdBy reads this one)", actor, systemActorPrefix)
	}
	if _, ok := auth.ClaimsFromContext(ctx); !ok {
		t.Error("claims missing -- BuildTokenInfo/createdBy derive from them")
	}
}
