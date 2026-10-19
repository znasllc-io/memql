package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// passkey_window_test.go -- identity's passkey window (memql#6233).
//
// The page exists to run a passkey request on identity's OWN origin, so what
// is pinned is that it stays there (every other identity page goes to OS),
// that it trusts exactly the installed OS origin to hand it a request, and
// that it carries the strict policy and no cached copy.

func passkeyWindowServer() *Server {
	return &Server{
		Cfg:    identity.Config{BaseURL: "https://identity.lab.example.com"},
		Logger: slog.Default(),
		Store:  &identity.Store{Engine: &setupEngine{}, Logger: slog.Default()},
	}
}

func getPath(s *Server, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	s.Mount(mux)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestThePasskeyWindowRendersOnIdentitysOwnOrigin(t *testing.T) {
	rec := getPath(passkeyWindowServer(), PasskeyWindowPath)
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("status = %d, Location = %q: the window must not be sent to OS", rec.Code, rec.Header().Get("Location"))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-opener-origins="https://os.lab.example.com"`) {
		t.Errorf("the window must trust exactly the installed OS origin:\n%s", body)
	}
	if !strings.Contains(body, "/static/passkey-window.js") {
		t.Errorf("the window lost its script:\n%s", body)
	}
	if cache := rec.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Errorf("Cache-Control = %q: a passkey page must not be cached", cache)
	}
	policy := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Errorf("the window's policy lost %q:\n  %s", directive, policy)
		}
	}
}

// THE CONTROL: the exemption is the window's alone. Another identity page
// still sends a browser to OS.
func TestOtherIdentityPagesStillOpenInOS(t *testing.T) {
	rec := getPath(passkeyWindowServer(), "/logout-complete")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "https://os.lab.example.com/identity/logout-complete") {
		t.Fatalf("status = %d, Location = %q: identity pages open in OS", rec.Code, rec.Header().Get("Location"))
	}
}

// The window holds nothing in its URL: a request arrives by message, so a
// query string can neither carry one nor change who is trusted.
func TestThePasskeyWindowIgnoresItsQuery(t *testing.T) {
	plain := getPath(passkeyWindowServer(), PasskeyWindowPath).Body.String()
	asked := getPath(passkeyWindowServer(), PasskeyWindowPath+"?opener=https://evil.example&authorization=Bearer+x").Body.String()
	if strings.Contains(asked, "evil.example") || strings.Contains(asked, "Bearer") {
		t.Errorf("the window echoed its query into the page:\n%s", asked)
	}
	if len(plain) == 0 || !strings.Contains(asked, `data-opener-origins="https://os.lab.example.com"`) {
		t.Errorf("a query changed who the window trusts:\n%s", asked)
	}
}
