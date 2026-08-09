package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// envFrom builds an env lookup over a fixed map, so these tests never mutate
// the process environment (which would make them order-dependent and unable
// to run in parallel with anything else in the package).
func envFrom(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestRuntimeConfigIsServedBeforeTheSPAFallback(t *testing.T) {
	resp := get(t, newTestHandler(t), "/"+runtimeConfigFile)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		// The realistic regression is the SPA fallback swallowing this path and
		// returning index.html, which fetch() reports as a JSON parse error --
		// a message about syntax for what is a routing bug.
		t.Fatalf("Content-Type = %q, want application/json; the SPA fallback has "+
			"probably swallowed the runtime-config path", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store -- a cached copy points the "+
			"browser at a stale identity service after a reconfiguration", cc)
	}
	var cfg RuntimeConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// The document is read before the operator has any credential, so it must
// never grow a field that is one. This asserts the whole serialized surface,
// which is what makes it a real guard: adding a field fails here until someone
// looks at this list and decides the new field is public.
func TestRuntimeConfigCarriesNoCredential(t *testing.T) {
	body, err := json.Marshal(runtimeConfigFromEnv(envFrom(map[string]string{
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity.example.com",
	})))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]bool{
		"identityUrl": true, "identityApiBaseUrl": true,
		"oauthClientId": true, "authEnabled": true,
	}
	for k := range fields {
		if !want[k] {
			t.Errorf("runtime-config.json grew field %q. It is served UNAUTHENTICATED "+
				"on a public path -- confirm the value is public (an issuer URL, a "+
				"public OAuth client id) and add it to this list, or take it out.", k)
		}
	}
	for k := range want {
		if _, ok := fields[k]; !ok {
			t.Errorf("runtime-config.json lost field %q; the bundle reads it", k)
		}
	}
}

// The in-cluster verifier URL must never be handed to a browser: it resolves
// only on the pod network, so using it would work on a single-host laptop and
// fail on every real deployment.
func TestIdentityURLPrefersTheIssuerNotTheInClusterVerifierURL(t *testing.T) {
	got := identityURL(envFrom(map[string]string{
		"MEMQL_IDENTITY_VERIFIER_BASE_URL":        "https://identity:8085",
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity.local.znas.io",
	}))
	if got != "https://identity.local.znas.io" {
		t.Fatalf("identityURL = %q, want the issuer", got)
	}
}

func TestIdentityURLOverrideAndFallbackOrder(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "explicit override wins",
			env: map[string]string{
				IdentityURLEnvVar:                         "https://login.example.com",
				"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": "https://identity.example.com",
			},
			want: "https://login.example.com",
		},
		{
			name: "identity binary's own base URL is the last resort",
			env:  map[string]string{"MEMQL_IDENTITY_BASE_URL": "https://identity.example.com/"},
			want: "https://identity.example.com",
		},
		{
			name: "nothing configured yields empty, not a guess",
			env:  map[string]string{},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identityURL(envFrom(tc.env)); got != tc.want {
				t.Fatalf("identityURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIdentityAPIBaseURLSelfSentinelMeansSameOrigin(t *testing.T) {
	got := identityAPIBaseURL(
		envFrom(map[string]string{IdentityAPIBaseURLEnvVar: "self"}),
		"https://identity.example.com",
	)
	if got != "" {
		t.Fatalf("identityApiBaseUrl = %q, want empty (same-origin)", got)
	}
}

func TestIdentityAPIBaseURLDefaultsToTheIdentityOrigin(t *testing.T) {
	got := identityAPIBaseURL(envFrom(map[string]string{}), "https://identity.example.com")
	if got != "https://identity.example.com" {
		t.Fatalf("identityApiBaseUrl = %q, want the identity origin", got)
	}
}

func TestOAuthClientIDDefaultAndOverride(t *testing.T) {
	if got := oauthClientID(envFrom(map[string]string{})); got != DefaultOAuthClientID {
		t.Fatalf("oauthClientId = %q, want %q", got, DefaultOAuthClientID)
	}
	if got := oauthClientID(envFrom(map[string]string{OAuthClientIDEnvVar: "ops-console"})); got != "ops-console" {
		t.Fatalf("oauthClientId = %q, want the override", got)
	}
}

// A node with no bundle answers 404 for everything under the mount, config
// included. One rule beats two: "no portal here" reads the same at every path.
func TestRuntimeConfigIs404WithoutABundle(t *testing.T) {
	h := New(Options{FS: fstest.MapFS{}, Logger: quiet()})
	resp := get(t, h, "/"+runtimeConfigFile)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// connect-src must name the identity origin or the token exchange is blocked
// in the browser -- and blocked invisibly from the server's side, since the
// request never leaves the page.
func TestCSPNamesTheIdentityOriginForTheTokenExchange(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "cockpit.example.com"
	policy := policyWith(r, "https://identity.example.com")
	if !strings.Contains(policy, "https://identity.example.com") {
		t.Fatalf("connect-src omits the identity origin; the OAuth token exchange "+
			"would be blocked by CSP.\npolicy = %s", policy)
	}
	if strings.Contains(policy, "identity.example.com/") {
		t.Errorf("CSP source carries a path; only the origin belongs there.\npolicy = %s", policy)
	}
}

func TestCSPDropsAHostileIdentityURL(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "cockpit.example.com"
	for _, raw := range []string{
		"https://evil.example.com\r\nX-Injected: 1",
		"https://evil.example.com foo",
		"javascript:alert(1)",
		"not a url at all",
	} {
		policy := policyWith(r, raw)
		if strings.ContainsAny(policy, "\r\n") {
			t.Fatalf("CSP contains a newline for identity URL %q -- header injection", raw)
		}
		if strings.Contains(policy, "evil.example.com") {
			t.Errorf("CSP admitted a malformed identity URL %q:\n%s", raw, policy)
		}
	}
}

// Same-origin deployments (the front-door proxy topology, and the
// single-binary build) must not get a redundant duplicate source.
func TestCSPOmitsTheIdentityOriginWhenItIsAlreadySelf(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "cockpit.example.com"

	sameOrigin := policyWith(r, "http://cockpit.example.com")
	if strings.Count(sameOrigin, "cockpit.example.com") != 1 {
		// The one occurrence is the ws:// source; 'self' already covers the
		// http origin.
		t.Fatalf("expected no duplicate http source for the portal's own origin:\n%s", sameOrigin)
	}

	proxied := policyWith(r, "")
	if !strings.Contains(proxied, "connect-src 'self' ws://cockpit.example.com;") &&
		!strings.HasSuffix(proxied, "connect-src 'self' ws://cockpit.example.com") {
		t.Fatalf("proxied (empty identity API base) policy should stop at 'self' + ws:\n%s", proxied)
	}
}
