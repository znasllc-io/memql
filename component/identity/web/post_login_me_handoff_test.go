package web

// First-party /me/* post-login handoff.
//
// /me/devices is where passkey registration lives. A signed-out visitor
// must come back THERE after the magic-link round trip, not to the OS
// shell. These tests pin the two places that park the destination and
// the one place that honours it for an already-signed-in browser.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

func TestLoginGetParksSameOriginReturnTo(t *testing.T) {
	s := newPasskeyLoginPageServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?return_to=/me/devices", nil)
	s.handleLoginGet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var postLogin string
	for _, c := range rec.Result().Cookies() {
		if c.Name == identity.PostLoginCookieName {
			postLogin = c.Value
		}
	}
	if postLogin != "/me/devices" {
		t.Fatalf("post-login cookie = %q, want /me/devices (covers the app.js 401 bounce that cannot set HttpOnly cookies)", postLogin)
	}
}

func TestLoginGetIgnoresAbsoluteReturnTo(t *testing.T) {
	s := newPasskeyLoginPageServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?return_to=https://evil.test/callback", nil)
	s.handleLoginGet(rec, req)

	for _, c := range rec.Result().Cookies() {
		if c.Name == identity.PostLoginCookieName {
			t.Fatalf("parked absolute return_to as post-login cookie %q; that would be an open redirect", c.Value)
		}
	}
}

func TestRedirectIfAuthenticatedSendsSignedInBrowserToSameOriginReturnTo(t *testing.T) {
	// Drive sessionClaims through a real issuer so the middleware sees a
	// signed-in caller. mintSSOAuthCode stays nil so the OAuth short-circuit
	// cannot fire; the same-origin branch is what must answer.
	s, _, signIn := passkeyServer(t, aliceOwnsOnePasskey())
	s.mintSSOAuthCode = nil

	called := false
	wrapped := s.redirectIfAuthenticated("/me", func(http.ResponseWriter, *http.Request) {
		called = true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?return_to=/me/devices", nil)
	signIn(req, aliceId)
	wrapped(rec, req)

	if called {
		t.Fatal("login form ran for a signed-in visitor with a same-origin return_to; that is the snap-back")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/me/devices" {
		t.Fatalf("Location = %q, want /me/devices", loc)
	}
}

func TestRedirectIfAuthenticatedStillFallsThroughForUnmatchedAbsoluteReturnTo(t *testing.T) {
	s, _, signIn := passkeyServer(t, aliceOwnsOnePasskey())
	s.mintSSOAuthCode = nil

	called := false
	wrapped := s.redirectIfAuthenticated("/me", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?return_to=https://unregistered.example/callback", nil)
	signIn(req, aliceId)
	wrapped(rec, req)

	if !called {
		t.Fatal("unmatched absolute return_to must still render the form; an untrusted RP must not get a silent bounce")
	}
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("unexpected redirect to %q", rec.Header().Get("Location"))
	}
}
