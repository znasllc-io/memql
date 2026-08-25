package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// These tests guard the memql#4302 Chrome regression: the device-bound
// magic-link flow ends in a form POST answered with a 303 to the OAuth
// client's origin, and Chrome enforces form-action against the whole
// redirect chain -- so under `form-action 'self'` the server-side flow
// succeeds completely while the browser cancels the final navigation and
// the Confirm button sits on "Confirming..." forever. No Go test can watch
// a browser enforce CSP; what CAN be pinned is the header the pages ship.

func TestClientFormActionOriginsReduceRedirectURIsToOrigins(t *testing.T) {
	clients := []identity.RegisteredClient{
		{ClientId: "portal", RedirectURIs: []string{
			"https://portal.memql.example/auth/callback",
			"https://portal.memql.example/auth/callback?flavor=b", // same origin, must dedupe
		}},
		{ClientId: "dev", RedirectURIs: []string{"http://localhost:5173/cb"}},
		{ClientId: "broken", RedirectURIs: []string{"not a uri", "/relative/only", ""}},
	}
	got := clientFormActionOrigins(clients)
	want := []string{"http://localhost:5173", "https://portal.memql.example"}
	if len(got) != len(want) {
		t.Fatalf("origins = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("origins = %v, want %v (sorted, deduplicated, invalid dropped)", got, want)
		}
	}
}

func TestPolicyExtendsExactlyTheFormActionDirective(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/auth/complete", nil)
	got := policyForOrigins(r, []string{"https://portal.memql.example"})

	if !strings.Contains(got, "form-action 'self' https://portal.memql.example") {
		t.Fatalf("form-action not extended: %q", got)
	}
	// Every OTHER directive stays byte-identical to the strict base --
	// extending form-action must never loosen script-src, connect-src, or
	// anything else.
	for _, directive := range []string{
		"default-src 'self'",
		"script-src 'self'",
		"connect-src 'self'",
		"base-uri 'self'",
		"object-src 'none'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(got, directive+";") && !strings.HasSuffix(got, directive) {
			t.Errorf("directive %q lost or altered in %q", directive, got)
		}
	}
	// The portal origin must appear ONLY in form-action.
	if strings.Count(got, "portal.memql.example") != 1 {
		t.Errorf("client origin leaked beyond form-action: %q", got)
	}
}

func TestPolicyWithoutClientsIsTheStrictBase(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/login", nil)
	if got, want := policyForOrigins(r, nil), policyFor(r); got != want {
		t.Fatalf("no clients must mean the unchanged base policy; got %q want %q", got, want)
	}
}

// TestWebUIRoutesCarryClientOriginsInFormAction pins the WIRING through the
// real mount, not the helper: a Server whose Cfg registers a client must emit
// the extended header on the pages that host the sign-in forms. This is the
// test that fails if the route wrap ever reverts to the package-level
// CSPHandlerFunc.
func TestWebUIRoutesCarryClientOriginsInFormAction(t *testing.T) {
	fx := newFlowFixture(t)
	fx.server.Cfg.RegisteredClients = []identity.RegisteredClient{
		{ClientId: "portal", RedirectURIs: []string{"https://portal.memql.example/auth/callback"}},
	}
	mux := http.NewServeMux()
	fx.server.Mount(mux)

	for _, path := range []string{
		"/auth/complete?ml=plain-token",                          // hosts the Confirm form
		"/check-email?email=a%40b.test&request=" + testRequestId, // hosts the finish form
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		got := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(got, "form-action 'self' https://portal.memql.example") {
			t.Errorf("GET %s does not extend form-action with the registered client origin.\n"+
				"Without it, Chrome cancels the 303 that ends the magic-link flow -- the server\n"+
				"consumes the link, the auth code strands, and Confirm sticks on 'Confirming...'.\ngot: %q",
				path, got)
		}
	}
}
