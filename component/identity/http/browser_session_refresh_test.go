package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBrowserSessionLeavesRefreshCookie pins the passkey-registration
// refresh-loop fix: a first-party magic-link / passkey web login must
// leave memql_refresh, not only memql_admin.
//
// Without the refresh cookie, requireUser renders /me/devices (valid
// access JWT), app.js POSTs /auth/refresh, gets 401, redirects to
// /login?return_to=/me/devices, and redirectIfAuthenticated sends the
// still-signed-in browser straight back -- a non-stop refresh that
// makes "Add a passkey" unreachable after the #5272 handoff lands on
// the right page.
func TestBrowserSessionLeavesRefreshCookie(t *testing.T) {
	s, _, _ := newSessionRowServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/auth/landing", nil)
	req.Header.Set("User-Agent", "Safari/17")

	if err := s.StartBrowserSessionFor(rec, req, "v1:identity:user:u1", "team@acme.test", "admin_session_started"); err != nil {
		t.Fatalf("StartBrowserSessionFor: %v", err)
	}

	got := map[string]string{}
	for _, c := range rec.Result().Cookies() {
		got[c.Name] = c.Value
	}
	if got[adminCookieName] == "" {
		t.Fatal("missing memql_admin: requireUser / SSO would not see the session")
	}
	if got[refreshCookieName] == "" {
		t.Fatal("missing memql_refresh: /auth/refresh would 401 and app.js would redirect-loop with redirectIfAuthenticated")
	}
	if got[sessionMarkerCookieName] == "" {
		t.Fatal("missing memql_session marker: SPA bootstrap presence signal must accompany the refresh cookie")
	}
	if got[refreshCookieName] == got[adminCookieName] {
		t.Fatal("refresh cookie must not be the access JWT; /auth/refresh rotates a distinct opaque token")
	}
}
