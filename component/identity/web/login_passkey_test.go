package web

// The /login passkey control is a PROGRESSIVE ENHANCEMENT (memql#3407).
//
// That claim has three testable halves, and this file asserts each of
// them against the RENDERED page rather than against the template
// source:
//
//  1. the magic-link form is present either way, so the page stays
//     usable when nothing reveals the button;
//  2. the control ships `hidden`, so a browser with no WebAuthn -- or no
//     JavaScript at all -- shows the email form and nothing else;
//  3. the control carries the SAME in-flight OAuth context the
//     magic-link form's hidden inputs carry, and does not render at all
//     when there is no relying party for an auth code to be minted for.

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

func newPasskeyLoginPageServer(t *testing.T) *Server {
	t.Helper()
	cfg := identity.Config{
		Enabled:     true,
		BaseURL:     "https://identity.test",
		JWTAudience: "memql",
		RegisteredClients: []identity.RegisteredClient{
			{ClientId: "app", RedirectURIs: []string{"https://app.example.com/auth/callback"}},
		},
	}
	s, err := NewServer(cfg, slog.Default(), nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func renderLogin(t *testing.T, s *Server, q url.Values) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?"+q.Encode(), nil)
	s.handleLoginGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestLoginPage_PasskeyControlIsHiddenAndCarriesTheOAuthContext(t *testing.T) {
	s := newPasskeyLoginPageServer(t)
	body := renderLogin(t, s, url.Values{
		"client_id":    {"app"},
		"redirect_uri": {"https://app.example.com/auth/callback"},
		"state":        {"st-1"},
	})

	// The magic-link form is still the primary path.
	if !strings.Contains(body, `name="email"`) {
		t.Fatal("the magic-link email form must still render alongside the passkey control")
	}
	// The control exists, is hidden, and names the ceremony script.
	if !strings.Contains(body, "data-passkey-login") {
		t.Fatal("the passkey control did not render on a page carrying a relying party")
	}
	block := body[strings.Index(body, "data-passkey-login"):]
	if end := strings.Index(block, "</div>"); end >= 0 {
		block = block[:end]
	}
	if !strings.Contains(body, "hidden data-passkey-login") {
		t.Error("the passkey control must render hidden -- only passkey-login.js may reveal it")
	}
	if !strings.Contains(body, "/static/passkey-login.js") {
		t.Error("passkey-login.js must be loaded for the control to ever become visible")
	}
	for _, want := range []string{
		`data-client-id="app"`,
		`data-redirect-uri="https://app.example.com/auth/callback"`,
		`data-state="st-1"`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("passkey control is missing %s; block=%s", want, block)
		}
	}
}

// No relying party -> no control. A passkey assertion's whole output is
// an auth code for a client; with no client there is nothing for it to
// produce, and the magic-link form's admin-session path is the way in.
func TestLoginPage_PasskeyControlAbsentWithoutARelyingParty(t *testing.T) {
	s := newPasskeyLoginPageServer(t)
	body := renderLogin(t, s, url.Values{})

	if strings.Contains(body, "data-passkey-login") {
		t.Error("the passkey control must not render when no relying party is in scope")
	}
	if !strings.Contains(body, `name="email"`) {
		t.Error("the magic-link form must remain the way in")
	}
}
