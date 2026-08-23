package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// login_pkce_required_test.go -- memql#4303: "POST /login refuses a matched
// client that sent no challenge, with a 400 naming the omission."
//
// # Why this refusal has to be at the door
//
// /oauth/token no longer redeems a code with no PKCE challenge. Accepting the
// submission here would therefore mint a code the client could never exchange
// -- a sign-in that looks like it worked, sends a real email, and dead-ends
// silently at the callback. Refusing up front costs the caller a message that
// says what to fix; accepting costs them a support ticket about a link that
// "does nothing".

func newPKCELoginServer(t *testing.T) (*Server, *IssueMagicLinkInput) {
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
	captured := &IssueMagicLinkInput{}
	s.IssueMagicLink = func(_ context.Context, in IssueMagicLinkInput) (IssueMagicLinkResult, error) {
		*captured = in
		return IssueMagicLinkResult{RequestId: "req-1", BindingNonce: "nonce-1"}, nil
	}
	s.CountUsers = func(_ context.Context) (int, error) { return 1, nil }
	return s, captured
}

func postLogin(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleLoginPost(rec, req)
	return rec
}

func TestLoginRefusesAMatchedClientWithNoChallenge(t *testing.T) {
	s, captured := newPKCELoginServer(t)

	rec := postLogin(t, s, url.Values{
		"form":         {"email"},
		"email":        {"user@example.com"},
		"client_id":    {"app"},
		"redirect_uri": {"https://app.example.com/auth/callback"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. A matched client with no PKCE challenge would mint an "+
			"auth code /oauth/token now refuses to redeem -- a sign-in that appears to work and "+
			"dead-ends at the callback.", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "code_challenge") {
		t.Errorf("the refusal does not name the missing parameter; body=%s", rec.Body.String())
	}
	if captured.Email != "" {
		t.Error("the issuer ran despite the refusal -- no email should be sent for a request that " +
			"cannot complete")
	}
}

func TestLoginAcceptsAMatchedClientWithAChallenge(t *testing.T) {
	s, captured := newPKCELoginServer(t)

	rec := postLogin(t, s, url.Values{
		"form":                  {"email"},
		"email":                 {"user@example.com"},
		"client_id":             {"app"},
		"redirect_uri":          {"https://app.example.com/auth/callback"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
		"code_challenge_method": {"S256"},
	})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if captured.CodeChallenge == "" {
		t.Error("the challenge did not reach the issuer")
	}
	// The binding cookie is set on the way out -- this is the same assertion
	// the flow tests make, from the issue side.
	var bound bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == magicLinkCookieName && c.Value == "nonce-1" {
			bound = true
			if c.Path != magicLinkCookiePath {
				t.Errorf("binding cookie Path = %q, want %q -- it must cover the four flow routes "+
					"and nothing else", c.Path, magicLinkCookiePath)
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Error("binding cookie is not SameSite=Lax; a mail client opens the link as a " +
					"top-level navigation, and Lax is the mode that sends the cookie on one")
			}
			if !c.HttpOnly {
				t.Error("binding cookie is not HttpOnly")
			}
		}
	}
	if !bound {
		t.Fatal("POST /login did not set the binding cookie.\n" +
			"Without it the emailed link can be APPROVED from anywhere and COMPLETED nowhere -- " +
			"which is the safe direction, but it means nobody can sign in.")
	}
	// The request id rides to /check-email so the poller has something to ask
	// about; it is not a credential.
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "request=req-1") {
		t.Errorf("redirect %q does not carry the request id; the poller has nothing to ask about", loc)
	}
}

// TestLoginWithNoMatchedClientNeedsNoChallenge pins the boundary: the
// admin-session path mints no OAuth code, so there is nothing for a PKCE
// verifier to protect and demanding one would break signing in to identity's
// own pages.
func TestLoginWithNoMatchedClientNeedsNoChallenge(t *testing.T) {
	s, captured := newPKCELoginServer(t)

	rec := postLogin(t, s, url.Values{
		"form":  {"email"},
		"email": {"user@example.com"},
	})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if !captured.AdminSession {
		t.Error("a /login with no relying party should be an admin-session issue")
	}
}
