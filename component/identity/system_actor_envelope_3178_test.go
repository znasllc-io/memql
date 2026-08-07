package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// system_actor_envelope_3178_test.go pins the fact memql#3178's admin-gated
// query depends on: the operator entry points resolve an ACTOR ENVELOPE, not
// just a claims map.
//
// `requiresOwnerOrAdmin` (dsl/common/specs.memql) is `role == "admin" || role
// == "owner"` over the @actor shape, and `actor.role` resolves from the
// AccessContext via auth.ActorEnvelopeValue -- never from claims. Before #3178
// ContextWithSystemActor set claims saying role="owner" and left the
// AccessContext nil, so the envelope reported role "" and the gate denied. The
// claims were right and the thing the gate reads was empty, which is why
// reading the claims map in a test would have proved nothing.
//
// These assertions go through ActorEnvelopeValue -- the same function the
// engine calls -- rather than reading AccessContext fields directly, so they
// fail if that resolution changes underneath them.

func actorEnvelope(t *testing.T, ctx context.Context, path string) any {
	t.Helper()
	ac, _ := auth.AccessFromContext(ctx)
	v, ok := auth.ActorEnvelopeValue(ac, path)
	if !ok {
		t.Fatalf("actor envelope has no %q path", path)
	}
	return v
}

// The operator CLI (`memql pat list --user-id X`, subcommand_pat.go) runs
// unauthenticated by design and stamps this actor. It must satisfy
// requiresOwnerOrAdmin or the admin arm of the #3178 split returns zero rows to
// the operator.
func TestContextWithSystemActorResolvesOwnerInTheActorEnvelope(t *testing.T) {
	ctx := ContextWithSystemActor(context.Background())

	if got := actorEnvelope(t, ctx, "role"); got != "owner" {
		t.Errorf("actor.role = %q, want \"owner\" -- requiresOwnerOrAdmin reads this, so the "+
			"operator CLI would get ZERO rows from patIdentitiesForUser (memql#3178)", got)
	}
	if got := actorEnvelope(t, ctx, "userId"); got != SystemActorSubject {
		t.Errorf("actor.userId = %q, want %q", got, SystemActorSubject)
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext on the system-actor context -- claims alone are invisible to " +
			"every actor.* reference in a DSL filter (memql#3178)")
	}
	if !ac.IsClusterOwner() {
		t.Error("system actor is not a cluster owner; admin-tier constructs gated on " +
			"actor.isClusterOwner would deny the operator")
	}
}

// The guard must not be lost: a context that already carries a real caller is
// returned untouched, so this helper can never ESCALATE somebody to owner.
func TestContextWithSystemActorDoesNotEscalateAnExistingActor(t *testing.T) {
	realCaller := auth.ContextWithUserActor(context.Background(), "v1:identity:user:alice")
	got := ContextWithSystemActor(realCaller)

	if v := actorEnvelope(t, got, "userId"); v != "v1:identity:user:alice" {
		t.Errorf("actor.userId = %q, want the original caller -- ContextWithSystemActor "+
			"overwrote a real actor", v)
	}
	if v := actorEnvelope(t, got, "role"); v == "owner" {
		t.Error("a real non-owner caller was ESCALATED to owner by ContextWithSystemActor")
	}
}

// SystemActorMiddleware is the HTTP twin, and it is deliberately NOT changed by
// #3178: it fronts identity's unauthenticated bootstrap endpoints (magic-link
// issue, token exchange, refresh), so granting them an owner AccessContext
// would hand an admin-gated query to any anonymous caller. It stamps claims
// only, and that is the intended asymmetry -- recorded here so a future
// "consistency" cleanup has to argue with a test rather than a comment.
func TestSystemActorMiddlewareDoesNotGrantAnOwnerAccessContext(t *testing.T) {
	var seen context.Context
	h := SystemActorMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r.Context()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if seen == nil {
		t.Fatal("middleware did not call through")
	}
	if ac, ok := auth.AccessFromContext(seen); ok && ac != nil && ac.IsClusterOwner() {
		t.Error("SystemActorMiddleware granted an owner AccessContext to an unauthenticated " +
			"HTTP request -- every admin-gated query would open up to anonymous callers " +
			"(memql#3178). The operator CLI gets its envelope from ContextWithSystemActor; " +
			"this middleware must not.")
	}
}
