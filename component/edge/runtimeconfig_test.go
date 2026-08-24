// component/edge/runtimeconfig_test.go
package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

	got := runtimeConfigForSite(context.Background(), site, env, true, nil)

	want := RuntimeConfig{
		IdentityURL:        "https://identity.example.com",
		IdentityAPIBaseURL: "",
		OAuthClientID:      "shop",
		AuthEnabled:        true,
		Domain:             "",
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
	got := runtimeConfigForSite(context.Background(), &Site{Hostname: "shop.example.com"}, env, true, nil)
	if got.IdentityURL != "https://identity-fallback.example.com" {
		t.Errorf("IdentityURL = %q, want the MEMQL_IDENTITY_BASE_URL fallback", got.IdentityURL)
	}
}

// A nil site (an unresolved host, defensively) must not panic -- the
// document just carries no matched client id.
func TestRuntimeConfigForSite_NilSiteDoesNotPanic(t *testing.T) {
	got := runtimeConfigForSite(context.Background(), nil, fakeEnv(nil), false, nil)
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

func TestRuntimeConfigForSite_CarriesTheDomain(t *testing.T) {
	env := fakeEnv(map[string]string{
		"MEMQL_DOMAIN":            "  acme.example.com ",
		"MEMQL_IDENTITY_BASE_URL": "https://identity.acme.example.com",
	})
	got := runtimeConfigForSite(context.Background(), &Site{ID: "s1", Hostname: "shop.acme.example.com"}, env, true, nil)
	if got.Domain != "acme.example.com" {
		t.Errorf("Domain = %q, want the trimmed MEMQL_DOMAIN", got.Domain)
	}
}

// An unset MEMQL_DOMAIN is served as an empty string, never omitted: a reader
// can then tell "this node predates the field" (key absent) from "this node
// has no domain" (key present, empty).
func TestServeRuntimeConfig_DomainKeyIsAlwaysPresent(t *testing.T) {
	doc := runtimeConfigForSite(context.Background(), &Site{ID: "s1", Hostname: "x"}, fakeEnv(map[string]string{}), false, nil)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	v, present := m["domain"]
	if !present {
		t.Fatalf("domain key missing from %s", raw)
	}
	if v != "" {
		t.Errorf("domain = %v, want empty string", v)
	}
}

// ---------------------------------------------------------------------------
// shopify_storefront binding (memql#4345, design D4)
// ---------------------------------------------------------------------------

// storefrontSecrets is a fake SecretResolver over a fixed map. It holds BOTH
// tokens a real cluster would hold for a store -- the public Storefront one
// and the Admin one -- because the interesting assertion is not that the
// resolver can find a token, it is that only ONE of them ever reaches the
// served document even though both are one lookup away.
func storefrontSecrets(t *testing.T) (SecretResolver, map[string]string) {
	t.Helper()
	secrets := map[string]string{
		"acme_storefront_token": "shpat_PUBLIC_STOREFRONT_TOKEN",
		"acme_admin_token":      "shpat_ADMIN_TOKEN_MUST_NEVER_BE_SERVED",
	}
	return func(_ context.Context, name string) (string, error) {
		v, ok := secrets[name]
		if !ok {
			return "", fmt.Errorf("no secret named %q", name)
		}
		return v, nil
	}, secrets
}

func storefrontSite() *Site {
	return &Site{
		ID:       "s-store",
		Hostname: "shop.acme.example.com",
		Kind:     "shopify_storefront",
		Status:   "live",
		Binding: map[string]any{
			"storeDomain":        "acme-demo.myshopify.com",
			"storefrontTokenRef": "acme_storefront_token",
		},
	}
}

// The whole point of D4's runtime half: a storefront site's document carries
// {kind, storeDomain, storefrontToken}, with the token RESOLVED from the
// globalSecret the row only NAMES.
func TestRuntimeConfigForSite_StorefrontCarriesTheResolvedBinding(t *testing.T) {
	resolve, _ := storefrontSecrets(t)

	got := runtimeConfigForSite(context.Background(), storefrontSite(), fakeEnv(nil), true, resolve)

	if got.Storefront == nil {
		t.Fatal("a shopify_storefront site got no storefront block")
	}
	if got.Storefront.Kind != "shopify_storefront" {
		t.Errorf("storefront.kind = %q, want shopify_storefront", got.Storefront.Kind)
	}
	if got.Storefront.StoreDomain != "acme-demo.myshopify.com" {
		t.Errorf("storefront.storeDomain = %q, want the binding's value", got.Storefront.StoreDomain)
	}
	if got.Storefront.StorefrontToken != "shpat_PUBLIC_STOREFRONT_TOKEN" {
		t.Errorf("storefront.storefrontToken = %q, want the resolved secret", got.Storefront.StorefrontToken)
	}
}

// A spa / static site's document is unchanged -- the key is absent, not
// present-and-empty, so an older cached bundle sees exactly what it saw
// before this field existed.
func TestRuntimeConfigForSite_NonStorefrontKindsCarryNothingNew(t *testing.T) {
	resolve, _ := storefrontSecrets(t)
	for _, kind := range []string{"spa", "static", ""} {
		site := &Site{ID: "s1", Hostname: "app.example.com", Kind: kind}
		doc := runtimeConfigForSite(context.Background(), site, fakeEnv(nil), true, resolve)
		if doc.Storefront != nil {
			t.Errorf("kind %q got a storefront block: %+v", kind, doc.Storefront)
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "storefront") {
			t.Errorf("kind %q document mentions storefront: %s", kind, raw)
		}
	}
}

// KIND IS THE GATE. A binding written onto a row of some other kind -- by
// accident, or by someone probing -- resolves nothing and publishes nothing:
// the token is only ever fetched for a site DECLARED to be a storefront.
func TestRuntimeConfigForSite_BindingOnANonStorefrontKindResolvesNoSecret(t *testing.T) {
	var lookups []string
	resolve := func(_ context.Context, name string) (string, error) {
		lookups = append(lookups, name)
		return "shpat_LEAKED", nil
	}
	site := storefrontSite()
	site.Kind = "spa" // same binding, wrong kind

	doc := runtimeConfigForSite(context.Background(), site, fakeEnv(nil), true, resolve)

	if doc.Storefront != nil {
		t.Errorf("a spa row carrying a storefront binding got a storefront block: %+v", doc.Storefront)
	}
	if len(lookups) != 0 {
		t.Errorf("the secret store was consulted for a non-storefront site: %v", lookups)
	}
}

// THE GREP. The Admin API token is the credential that must never reach a
// browser -- it can read orders and customers and mutate the store, unlike
// the Storefront token, which Shopify designs to be published. The resolver
// here holds BOTH, so the assertion is not "the admin token was unavailable",
// it is "the admin token was available one lookup away and still did not
// appear in the bytes we served".
func TestRuntimeConfigNeverCarriesTheShopifyAdminToken(t *testing.T) {
	resolve, secrets := storefrontSecrets(t)
	admin := secrets["acme_admin_token"]

	h := NewHandler(Options{
		Resolver:       staticResolver{site: storefrontSite()},
		SecretResolver: resolve,
	})
	req := httptest.NewRequest(http.MethodGet, runtimeConfigPath, nil)
	req.Host = "shop.acme.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", runtimeConfigPath, rec.Code)
	}
	body := rec.Body.String()

	// The instrument can move: the PUBLIC token IS in this same document, so
	// a document that failed to carry any secret at all would fail here
	// first and this test could not pass vacuously.
	if !strings.Contains(body, secrets["acme_storefront_token"]) {
		t.Fatalf("the served document does not carry the public Storefront token, so the "+
			"admin-token check below would prove nothing: %s", body)
	}
	if strings.Contains(body, admin) {
		t.Errorf("the served runtime-config document carries the Shopify ADMIN token: %s", body)
	}
	if strings.Contains(strings.ToLower(body), "admin") {
		t.Errorf("the served runtime-config document mentions an admin credential: %s", body)
	}
}

// An unresolvable ref is an empty token, never an error string in the
// document -- an error would tell every visitor the name of a secret and
// whether it exists.
func TestRuntimeConfigForSite_UnresolvableTokenRefIsAnEmptyToken(t *testing.T) {
	resolve := func(_ context.Context, name string) (string, error) {
		return "", fmt.Errorf("secret %q not found in the vault", name)
	}
	site := storefrontSite()

	doc := runtimeConfigForSite(context.Background(), site, fakeEnv(nil), true, resolve)

	if doc.Storefront == nil {
		t.Fatal("want a storefront block even when the token cannot be resolved")
	}
	if doc.Storefront.StorefrontToken != "" {
		t.Errorf("storefrontToken = %q, want empty", doc.Storefront.StorefrontToken)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "not found in the vault") {
		t.Errorf("the resolver's error leaked into the document: %s", raw)
	}
}

// A node with no secret resolver wired still serves the rest of the block.
func TestRuntimeConfigForSite_NoResolverStillCarriesTheStoreDomain(t *testing.T) {
	doc := runtimeConfigForSite(context.Background(), storefrontSite(), fakeEnv(nil), true, nil)
	if doc.Storefront == nil || doc.Storefront.StoreDomain != "acme-demo.myshopify.com" {
		t.Fatalf("storefront = %+v, want the store domain with no token", doc.Storefront)
	}
	if doc.Storefront.StorefrontToken != "" {
		t.Errorf("storefrontToken = %q, want empty with no resolver", doc.Storefront.StorefrontToken)
	}
}
