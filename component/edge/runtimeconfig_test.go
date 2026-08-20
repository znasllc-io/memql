// component/edge/runtimeconfig_test.go
package edge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeEnv is a tiny map-backed env func for exercising the pure derivation
// without touching the process environment.
func fakeEnv(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// registeredClientsFixture mirrors the shape component/envregistry/domain.go
// serializes into MEMQL_IDENTITY_REGISTERED_CLIENTS: an "app" site, a
// loopback "cockpit" native client, and an arbitrary hosted site -- proving
// the lookup is not keyed on any name this package recognises.
const registeredClientsFixture = `[` +
	`{"clientId":"app","redirectURIs":["https://app.example.com/auth/callback"]},` +
	`{"clientId":"cockpit","redirectURIs":["http://127.0.0.1/cockpit/callback"]},` +
	`{"clientId":"shop","redirectURIs":["https://shop.example.com/auth/callback"]}` +
	`]`

func TestClientIDForHostname_MatchesByExactCallback(t *testing.T) {
	got := clientIDForHostname("shop.example.com", registeredClientsFixture)
	if got != "shop" {
		t.Errorf("clientIDForHostname(shop.example.com) = %q, want %q", got, "shop")
	}
}

// The point of the whole design: a hostname the fixture never names resolves
// to "" through the SAME code path, not a different one.
func TestClientIDForHostname_NoMatchIsEmpty(t *testing.T) {
	got := clientIDForHostname("nobody-registered.example.com", registeredClientsFixture)
	if got != "" {
		t.Errorf("clientIDForHostname(unregistered host) = %q, want empty", got)
	}
}

func TestClientIDForHostname_EmptyInputsAreEmpty(t *testing.T) {
	if got := clientIDForHostname("", registeredClientsFixture); got != "" {
		t.Errorf("clientIDForHostname(empty hostname) = %q, want empty", got)
	}
	if got := clientIDForHostname("shop.example.com", ""); got != "" {
		t.Errorf("clientIDForHostname(no registered clients) = %q, want empty", got)
	}
}

// Malformed input must fail closed to "", never panic and never surface the
// raw env var to a caller as a client id.
func TestClientIDForHostname_MalformedJSONIsEmpty(t *testing.T) {
	got := clientIDForHostname("shop.example.com", "{not json")
	if got != "" {
		t.Errorf("clientIDForHostname(malformed JSON) = %q, want empty", got)
	}
}

// http, not https, must NOT match -- the lookup forces https to agree with
// csp.go's siteOriginOf, which is always https regardless of the request's
// own scheme.
func TestClientIDForHostname_RequiresHTTPS(t *testing.T) {
	clients := `[{"clientId":"shop","redirectURIs":["http://shop.example.com/auth/callback"]}]`
	if got := clientIDForHostname("shop.example.com", clients); got != "" {
		t.Errorf("clientIDForHostname matched an http:// redirect URI, want no match; got %q", got)
	}
}

func TestRuntimeConfigForSite_FullDerivation(t *testing.T) {
	env := fakeEnv(map[string]string{
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity.example.com",
		"MEMQL_IDENTITY_BASE_URL":                 "https://identity-fallback.example.com",
		"MEMQL_IDENTITY_REGISTERED_CLIENTS":       registeredClientsFixture,
	})
	site := &Site{ID: "s1", Hostname: "shop.example.com"}

	got := runtimeConfigForSite(site, env, true)

	want := RuntimeConfig{
		IdentityURL:        "https://identity.example.com",
		IdentityAPIBaseURL: "",
		OAuthClientID:      "shop",
		AuthEnabled:        true,
	}
	if got != want {
		t.Errorf("runtimeConfigForSite = %+v, want %+v", got, want)
	}
}

// The verifier-expected-issuer tier is preferred; MEMQL_IDENTITY_BASE_URL is
// the fallback when it is absent. Same priority component/portal/config.go
// used, ported rather than re-invented.
func TestRuntimeConfigForSite_FallsBackToBaseURL(t *testing.T) {
	env := fakeEnv(map[string]string{
		"MEMQL_IDENTITY_BASE_URL": "https://identity-fallback.example.com",
	})
	got := runtimeConfigForSite(&Site{Hostname: "shop.example.com"}, env, true)
	if got.IdentityURL != "https://identity-fallback.example.com" {
		t.Errorf("IdentityURL = %q, want the MEMQL_IDENTITY_BASE_URL fallback", got.IdentityURL)
	}
}

// A nil site (an unresolved host, defensively) must not panic -- the
// document just carries no matched client id.
func TestRuntimeConfigForSite_NilSiteDoesNotPanic(t *testing.T) {
	got := runtimeConfigForSite(nil, fakeEnv(nil), false)
	if got.OAuthClientID != "" {
		t.Errorf("OAuthClientID = %q, want empty for a nil site", got.OAuthClientID)
	}
}

// The HTTP-level wiring: GET /runtime-config.json for TWO DIFFERENT sites
// returns TWO DIFFERENT documents through the exact same handler code path
// -- proof this is generic per-site discovery, not a portal branch. One
// hostname is registered with identity, the other is not; both still get a
// 200 with the cluster-wide fields, differing only in oauthClientId.
func TestServeRuntimeConfig_IsGenericPerSite(t *testing.T) {
	t.Setenv("MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER", "https://identity.example.com")
	t.Setenv("MEMQL_IDENTITY_REGISTERED_CLIENTS", registeredClientsFixture)
	t.Setenv("MEMQL_IDENTITY_ENABLED", "true")

	for _, tc := range []struct {
		name        string
		site        *Site
		wantClient  string
		wantAuthURL string
	}{
		{"registered site", &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}, "shop", "https://identity.example.com"},
		{"unregistered site", &Site{ID: "s2", Hostname: "unregistered.example.com", Status: "live", Kind: "spa"}, "", "https://identity.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(Options{
				Resolver: staticResolver{site: tc.site},
				Opener:   mapOpener(map[string]string{"index.html": "ROOT"}),
			})
			req := httptest.NewRequest(http.MethodGet, runtimeConfigPath, nil)
			req.Host = tc.site.Hostname
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200; body: %s", runtimeConfigPath, rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}

			var doc RuntimeConfig
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
			}
			if doc.IdentityURL != tc.wantAuthURL {
				t.Errorf("identityUrl = %q, want %q", doc.IdentityURL, tc.wantAuthURL)
			}
			if doc.IdentityAPIBaseURL != "" {
				t.Errorf("identityApiBaseUrl = %q, want empty (same-origin XHR)", doc.IdentityAPIBaseURL)
			}
			if doc.OAuthClientID != tc.wantClient {
				t.Errorf("oauthClientId = %q, want %q", doc.OAuthClientID, tc.wantClient)
			}
			if !doc.AuthEnabled {
				t.Error("authEnabled = false, want true")
			}
		})
	}
}

// A draft site must not leak its runtime-config any more than it leaks its
// bundle -- the status gate in ServeHTTP runs before ANY path is
// dispatched, runtime-config.json included.
func TestServeRuntimeConfig_DraftSiteIs404(t *testing.T) {
	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{ID: "s1", Hostname: "shop.example.com", Status: "draft"}},
		Opener:   mapOpener(map[string]string{"index.html": "ROOT"}),
	})
	req := httptest.NewRequest(http.MethodGet, runtimeConfigPath, nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("draft site GET %s = %d, want 404", runtimeConfigPath, rec.Code)
	}
}

func TestServeRuntimeConfig_RefusesNonGET(t *testing.T) {
	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}},
		Opener:   mapOpener(map[string]string{"index.html": "ROOT"}),
	})
	req := httptest.NewRequest(http.MethodPost, runtimeConfigPath, nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST %s = %d, want 405", runtimeConfigPath, rec.Code)
	}
}
