package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

// dcr_off_signin_test.go -- memql#4518.
//
// THE LANE THIS CLOSES. The defect memql#4514 fixes shipped green because
// nothing exercised a FIRST sign-in against a cluster in its DEFAULT posture:
// MEMQL_IDENTITY_OAUTH_DCR_ENABLED unset, no static clients, no clusters.yaml
// clientId. Every test around this one configured a client -- statically, or
// through a DCR store row -- so the one shape a fresh cluster actually presents
// was the one shape nobody drove.
//
// WHY THIS IS AN IN-PROCESS TEST AND NOT A LIVE-CLUSTER ONE. The extension's
// test-host lane has a live half (editors/vscode/test-host/live.ts) which skips
// unless an operator points it at a credentialed cluster -- so it skips on every
// CI lane and every developer machine. A gate skipped by default cannot be what
// stands between a feature and the bug it prevents; this repo learned that in
// memql#4352 and put the real gate in an in-process hop test. Same reasoning
// here: this drives the REAL handler with a DCR-off configuration, so it runs
// everywhere, every time.
//
// The extension side of the same claim is asserted in TypeScript
// (editors/vscode/test/authWellKnownClient.test.ts, zero /register requests in
// both flows), and the two constants they must agree on are pinned to one
// fixture (test/fixtures/first-party-client-contract.json).

// newHardenedServer is a cluster in its default posture: DCR off, no static
// clients, and a NIL store -- so nothing can resolve out of the DCR store even
// in principle. Anything that resolves here resolves because it is compiled in.
func newHardenedServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		Cfg:    identity.Config{BaseURL: "http://localhost:8080"},
		Store:  nil,
		Logger: slog.Default(),
	}
}

// editorAuthorizeURL is the request the extension's browser flow opens: the
// well-known client id, and a loopback redirect on whatever ephemeral port the
// listener was handed.
func editorAuthorizeURL(port string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {identity.BuiltinClientVSCode},
		"redirect_uri":          {"http://127.0.0.1:" + port + "/callback"},
		"state":                 {"state-xyz"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
		"code_challenge_method": {"S256"},
	}
	return "/authorize?" + q.Encode()
}

func TestDCROff_EditorAuthorizeRendersTheConsentPage(t *testing.T) {
	s := newHardenedServer(t)

	rec := httptest.NewRecorder()
	s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, editorAuthorizeURL("54321"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /authorize: status = %d, want 200; body=%.800s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// The two failures this replaces, named so a regression says which:
	if strings.Contains(body, "Unknown client") {
		t.Fatal("the built-in editor client did not resolve -- this is the pre-epic failure, one layer in")
	}
	if strings.Contains(body, "Invalid redirect URI") {
		t.Fatal("the ephemeral-port callback was refused -- the registered URI has stopped being portless")
	}

	// The consent page must carry the OAuth context forward in hidden fields,
	// and must name the client the way a first-party client is named.
	for _, want := range []string{
		`name="client_id"`,
		`name="redirect_uri"`,
		`name="code_challenge"`,
		identity.BuiltinClientVSCode,
		"MemQL for VS Code",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page missing %q", want)
		}
	}
}

func TestDCROff_AnyEphemeralPortIsAccepted(t *testing.T) {
	// The bug this catches appears on the SECOND sign-in, never the first: the
	// listener gets a different port each time, so a registered URI that had
	// stopped being portless would still work once.
	s := newHardenedServer(t)
	for _, port := range []string{"1", "1024", "54321", "65535"} {
		rec := httptest.NewRecorder()
		s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, editorAuthorizeURL(port), nil))
		if rec.Code != http.StatusOK {
			t.Errorf("port %s: status = %d, want 200; body=%.300s", port, rec.Code, rec.Body.String())
		}
	}
}

func TestDCROff_TheConsentPageDoesNotCallTheEditorSelfRegistered(t *testing.T) {
	// memql#3794 renders a warning, and withholds a bundled logo, for a client
	// that described ITSELF at POST /register. A built-in is code rather than a
	// row, so it must render as first-party -- and the same page must still
	// treat an actual DCR client as self-registered, which
	// consent_unverified_test.go pins from the other side.
	s := newHardenedServer(t)
	rec := httptest.NewRecorder()
	s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, editorAuthorizeURL("54321"), nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%.300s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "NOT VERIFIED") {
		t.Error("the built-in editor client rendered as self-registered")
	}
}

func TestDCROff_AnUnknownClientIsStillRefused(t *testing.T) {
	// The built-in tier is additive. It must not turn /authorize into a page
	// that accepts any client_id somebody types.
	s := newHardenedServer(t)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {"not-a-client"},
		"redirect_uri":          {"http://127.0.0.1:54321/callback"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
		"code_challenge_method": {"S256"},
	}
	rec := httptest.NewRecorder()
	s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%.300s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Unknown client") {
		t.Errorf("body = %.300s, want the unknown-client page", rec.Body.String())
	}
}

func TestDCROff_AForeignRedirectIsStillRefusedForTheBuiltin(t *testing.T) {
	// The any-port exception is scoped to loopback. A built-in client must not
	// become a redirect to anywhere.
	s := newHardenedServer(t)
	for _, redirect := range []string{
		"https://evil.example.com/callback",
		"http://127.0.0.1:54321/other",
		"https://127.0.0.1:54321/callback",
	} {
		q := url.Values{
			"response_type":         {"code"},
			"client_id":             {identity.BuiltinClientVSCode},
			"redirect_uri":          {redirect},
			"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"},
			"code_challenge_method": {"S256"},
		}
		rec := httptest.NewRecorder()
		s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("redirect %q: status = %d, want 400", redirect, rec.Code)
		}
	}
}

func TestDCROff_PKCEIsStillRequiredForTheBuiltin(t *testing.T) {
	// "Do not weaken flows to make them drivable" (#4518): the built-in is a
	// PUBLIC client, so PKCE -- not a secret -- is what binds the code to the
	// process that asked for it.
	s := newHardenedServer(t)
	for _, extra := range []map[string]string{
		{},                      // no challenge at all
		{"code_challenge": "x"}, // no method
		{"code_challenge": "x", "code_challenge_method": "plain"}, // the wrong method
	} {
		q := url.Values{
			"response_type": {"code"},
			"client_id":     {identity.BuiltinClientVSCode},
			"redirect_uri":  {"http://127.0.0.1:54321/callback"},
			"state":         {"state-xyz"},
		}
		for k, v := range extra {
			q.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.handleAuthorize(rec, httptest.NewRequest(http.MethodGet, "/authorize?"+q.Encode(), nil))

		// A client + redirect that validated means the error leaves as an
		// OAuth redirect rather than a page -- which is the contract the
		// extension reads.
		if rec.Code != http.StatusFound {
			t.Fatalf("extra=%v: status = %d, want 302; body=%.300s", extra, rec.Code, rec.Body.String())
		}
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if loc.Query().Get("error") != "invalid_request" {
			t.Errorf("extra=%v: error = %q, want invalid_request", extra, loc.Query().Get("error"))
		}
		if loc.Query().Get("code") != "" {
			t.Errorf("extra=%v: a PKCE-less request received an authorization code", extra)
		}
	}
}
