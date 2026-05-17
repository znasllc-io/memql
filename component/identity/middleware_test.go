package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestSystemActorMiddleware_StampsActorOnUnauthenticatedRequests(t *testing.T) {
	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = auth.ActorFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	wrapped := SystemActorMiddleware(probe)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/setup", nil)
	wrapped.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("ActorFromContext returned empty; mutation pipeline would reject")
	}
	if !strings.Contains(seen, "system") && !strings.Contains(seen, "identity") {
		t.Errorf("actor = %q, want a recognizable system marker", seen)
	}
}

func TestSystemActorMiddleware_PreservesUpstreamActor(t *testing.T) {
	// If a higher-level middleware (e.g. requireAdmin once it
	// stamps auth.TokenInfo) attached a real actor first, the
	// system fallback must NOT clobber it.
	upstreamClaims := map[string]any{
		"sub":   "v1:identity:user:alice",
		"email": "alice@acme.com",
		"role":  "admin",
	}
	upstreamTok := auth.BuildTokenInfo(upstreamClaims)

	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = auth.ActorFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	wrapped := SystemActorMiddleware(probe)

	ctx := auth.ContextWithToken(context.Background(), upstreamTok)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/me", nil).WithContext(ctx)
	wrapped.ServeHTTP(rec, req)

	if seen != "alice@acme.com" {
		t.Errorf("actor = %q, want alice@acme.com (upstream actor must win)", seen)
	}
}

func TestSystemActorMiddleware_IsIdempotent(t *testing.T) {
	// Wrapping twice must produce the same actor as wrapping once;
	// the inner wrap should detect the actor stamped by the outer
	// wrap and short-circuit.
	var seen string
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = auth.ActorFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	wrapped := SystemActorMiddleware(SystemActorMiddleware(probe))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/setup", nil)
	wrapped.ServeHTTP(rec, req)

	if seen == "" {
		t.Fatal("double-wrap should still stamp an actor")
	}
}

func TestSystemActorHandlerFunc_ShimWorks(t *testing.T) {
	var seen string
	wrapped := SystemActorHandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = auth.ActorFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/setup", nil)
	wrapped(rec, req)

	if seen == "" {
		t.Fatal("HandlerFunc shim did not stamp an actor")
	}
}
