package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildSSORedirect_AppendsCodeAndState(t *testing.T) {
	got := buildSSORedirect("http://localhost:8080/auth/callback", "ABC123", "xyz")
	const want = "http://localhost:8080/auth/callback?code=ABC123&state=xyz"
	if got != want {
		t.Errorf("buildSSORedirect = %q, want %q", got, want)
	}
}

func TestBuildSSORedirect_OmitsEmptyState(t *testing.T) {
	got := buildSSORedirect("http://localhost:8080/auth/callback", "ABC123", "")
	const want = "http://localhost:8080/auth/callback?code=ABC123"
	if got != want {
		t.Errorf("buildSSORedirect = %q, want %q", got, want)
	}
}

func TestBuildSSORedirect_PreservesExistingQueryParams(t *testing.T) {
	got := buildSSORedirect("http://localhost:8080/auth/callback?foo=bar", "ABC", "S")
	for _, frag := range []string{"foo=bar", "code=ABC", "state=S"} {
		if !strings.Contains(got, frag) {
			t.Errorf("buildSSORedirect = %q, missing %q", got, frag)
		}
	}
}

// stubServerWithSession returns a *Server whose hasValidSession
// always reports the supplied value. This is the smallest seam that
// lets us test the middleware without standing up a full Issuer.
type stubSessionServer struct {
	*Server
	valid bool
}

// We can't override Server.hasValidSession without changing its
// receiver to take a sessionChecker; instead, drive the middleware
// via the public seams. The redirectIfAuthenticated branch is:
//
//	(a) return_to present -> always next
//	(b) hasValidSession    -> redirect to target
//	(c) otherwise          -> next
//
// (a) is independent of session state. (b)/(c) we cover by checking
// behavior with an unwired Server (meTokens=nil) which makes
// hasValidSession return false. Active-session (b) coverage rides
// the live verification at the end of the commit message.

func TestRedirectIfAuthenticated_ReturnToFallsThrough(t *testing.T) {
	s := &Server{} // unwired -- hasValidSession=false
	called := false
	wrapped := s.redirectIfAuthenticated("/admin/", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?return_to=http://localhost:8080/auth/callback", nil)
	wrapped(rec, req)

	if !called {
		t.Fatalf("handler should have been invoked when return_to is present")
	}
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("did not expect a 303 redirect on return_to flow, got %d", rec.Code)
	}
}

func TestRedirectIfAuthenticated_NoSessionFallsThrough(t *testing.T) {
	s := &Server{} // unwired -- hasValidSession returns false
	called := false
	wrapped := s.redirectIfAuthenticated("/admin/", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	wrapped(rec, req)

	if !called {
		t.Fatalf("handler should have been invoked when no valid session exists")
	}
}

func TestRedirectIfAuthenticated_NilNextIsHarmless(t *testing.T) {
	s := &Server{}
	wrapped := s.redirectIfAuthenticated("/admin/", nil)
	if wrapped != nil {
		t.Fatalf("expected nil wrapper for nil handler; got non-nil")
	}
}

func TestHasValidSession_NilDeps(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	if s.hasValidSession(req) {
		t.Fatalf("hasValidSession should be false when dependencies are unwired")
	}
}

func TestHasValidSession_NoToken(t *testing.T) {
	// Even with deps wired, missing cookie + no Bearer -> false.
	s := &Server{meTokens: &MeTokens{Issuer: nil}}
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	if s.hasValidSession(req) {
		t.Fatalf("hasValidSession should be false when no token is present")
	}
}
