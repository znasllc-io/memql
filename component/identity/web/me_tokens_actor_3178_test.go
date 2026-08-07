package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
)

// me_tokens_actor_3178_test.go pins the context /me/tokens hands the engine.
//
// The listing moved to the self-scoped `patIdentitiesForSelf`
// (`userId==actor.userId`) in memql#3178, which derives the row set from the
// ACTOR ENVELOPE. That envelope resolves from an auth.AccessContext and from
// nothing else -- identity.SystemActorMiddleware, which wraps every identity
// web route, stamps claims and a TokenInfo but no AccessContext, so without
// callerActorCtx the page would render empty for every signed-in user.
//
// Asserted through auth.ActorEnvelopeValue, the same resolver the engine uses.

// claimsFor builds verified-shaped access-token claims; Subject rides the
// embedded RegisteredClaims.
func claimsFor(subject string) *identity.AccessTokenClaims {
	return &identity.AccessTokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: subject},
	}
}

func TestCallerActorCtxPutsTheSignedInUserInTheActorEnvelope(t *testing.T) {
	// A request as it actually arrives: wrapped by SystemActorMiddleware, so it
	// already carries the system claims and NO AccessContext.
	var wrapped *http.Request
	identity.SystemActorMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		wrapped = r
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/me/tokens", nil))
	if wrapped == nil {
		t.Fatal("middleware did not call through")
	}

	// Baseline: this is why the stamp is needed, not merely tidy.
	if ac, ok := auth.AccessFromContext(wrapped.Context()); ok && ac != nil && ac.UserId != "" {
		t.Fatalf("precondition changed: the request already carries an AccessContext for %q; "+
			"re-check whether callerActorCtx is still the right fix (memql#3178)", ac.UserId)
	}

	const subject = "v1:identity:user:alice-3178"
	ctx := callerActorCtx(wrapped, claimsFor(subject))

	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("callerActorCtx produced no AccessContext -- patIdentitiesForSelf would resolve " +
			"actor.userId to \"\" and /me/tokens would render empty for everyone (memql#3178)")
	}
	got, _ := auth.ActorEnvelopeValue(ac, "userId")
	if got != subject {
		t.Errorf("actor.userId = %q, want %q -- the self-scoped listing keys on this", got, subject)
	}
}

// Defensive shape: the helper must never invent an actor from nothing. A nil
// claims pointer returns the request context untouched rather than stamping an
// empty subject, which auth.ContextWithUserActor would also refuse.
func TestCallerActorCtxWithoutClaimsStampsNoActor(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/me/tokens", nil)
	if ctx := callerActorCtx(r, nil); ctx != r.Context() {
		t.Error("callerActorCtx stamped an actor without verified claims")
	}
	if ctx := callerActorCtx(nil, nil); ctx == nil {
		t.Error("callerActorCtx(nil, nil) returned a nil context")
	}
	var _ context.Context = callerActorCtx(r, claimsFor(""))
}
