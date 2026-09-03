// component/edge/runtimeconfig_test.go
package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
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
		Settings:           map[string]string{},
	}
	// reflect.DeepEqual rather than !=: Settings is a map, so the struct is
	// no longer comparable, and a nil-vs-empty map difference here would be
	// exactly the "always carries the key" property the settings tests pin.
	if !reflect.DeepEqual(got, want) {
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

// TestClientIDForHostname_OsHostResolvesToPortalClient is memql#4705: the OS
// shell reuses the portal OAuth client. Once DomainDerivations registers
// https://os.<d>/auth/callback on that client, the edge lookup -- which keys
// on hostname, not a name it recognises -- must return "portal".
func TestClientIDForHostname_OsHostResolvesToPortalClient(t *testing.T) {
	// Imported via the same JSON DomainDerivations writes, not a hand-built
	// fixture: a second spelling here would pass while production disagreed.
	// The test lives in this package because clientIDForHostname is what the
	// browser reads as oauthClientId.
	clients := `[{"clientId":"app","redirectURIs":["https://app.example.com/auth/callback"]},` +
		`{"clientId":"cockpit","redirectURIs":["http://127.0.0.1/cockpit/callback"]},` +
		`{"clientId":"portal","redirectURIs":["https://portal.example.com/auth/callback","https://os.example.com/auth/callback"]}]`
	if got := clientIDForHostname("os.example.com", clients); got != "portal" {
		t.Errorf("clientIDForHostname(os.example.com) = %q, want portal -- the OS reuses the portal client", got)
	}
	if got := clientIDForHostname("portal.example.com", clients); got != "portal" {
		t.Errorf("clientIDForHostname(portal.example.com) = %q, want portal still", got)
	}
}

// ---------------------------------------------------------------------------
// Runtime settings (epic memql#4906, decision P7)
// ---------------------------------------------------------------------------

// The document ALWAYS carries `settings`, as an empty object when the row has
// none -- unlike `storefront`, which is kind-specific and absent by design. A
// bundle reads config.settings.<key> without first asking whether the object
// exists.
func TestRuntimeConfigForSite_SettingsKeyIsAlwaysPresent(t *testing.T) {
	for name, site := range map[string]*Site{
		"nil site":        nil,
		"no settings":     {ID: "s1", Hostname: "app.example.com", Kind: "spa"},
		"empty settings":  {ID: "s1", Hostname: "app.example.com", Kind: "spa", Settings: map[string]string{}},
		"static kind":     {ID: "s2", Hostname: "docs.example.com", Kind: "static"},
		"storefront kind": {ID: "s3", Hostname: "shop.example.com", Kind: "shopify_storefront"},
	} {
		doc := runtimeConfigForSite(context.Background(), site, fakeEnv(nil), true, nil)
		if doc.Settings == nil {
			t.Errorf("%s: Settings is nil; the document must always carry the key", name)
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"settings":{}`) {
			t.Errorf("%s: document does not carry an empty settings object: %s", name, raw)
		}
	}
}

// The shape a bundle reads: the row's values, verbatim, under `settings`,
// after the storefront block. Pinned on the SERVED bytes, not the struct, since
// the bytes are the contract an older cached bundle keeps working against.
func TestServeRuntimeConfig_CarriesTheSettings(t *testing.T) {
	site := &Site{
		ID: "s1", Hostname: "app.example.com", Status: "live", Kind: "spa",
		Settings: map[string]string{"apiBase": "https://api.acme.example", "region": "eu"},
	}
	h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: mapOpener{"index.html": "ROOT"}})
	req := httptest.NewRequest(http.MethodGet, runtimeConfigPath, nil)
	req.Host = site.Hostname
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", runtimeConfigPath, rec.Code)
	}
	var doc struct {
		Settings map[string]string `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Settings["apiBase"] != "https://api.acme.example" || doc.Settings["region"] != "eu" {
		t.Errorf("settings = %v, want the row's values verbatim", doc.Settings)
	}
	if len(doc.Settings) != 2 {
		t.Errorf("settings carries %d keys, want exactly the row's 2", len(doc.Settings))
	}
}

// ONE BUNDLE, TWO DEPLOYABLES, TWO DOCUMENTS. The bytes served are the same
// for both hosts; only the runtime-config document differs, which is the
// whole reason settings exist (P7: "one bundle can serve two deployables
// against different endpoints without a rebuild").
func TestServeRuntimeConfig_TwoDeployablesOneBundleReadDifferentSettings(t *testing.T) {
	bundle := mapOpener{"index.html": "SAME-BYTES"}
	sites := map[string]*Site{
		"eu.example.com": {ID: "eu", Hostname: "eu.example.com", Status: "live", Kind: "spa", BundleRef: "blob://sites/shared/v1/",
			Settings: map[string]string{"apiBase": "https://api.eu.example"}},
		"us.example.com": {ID: "us", Hostname: "us.example.com", Status: "live", Kind: "spa", BundleRef: "blob://sites/shared/v1/",
			Settings: map[string]string{"apiBase": "https://api.us.example"}},
	}
	got := map[string]string{}
	for host, site := range sites {
		h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: bundle})

		page := httptest.NewRequest(http.MethodGet, "/", nil)
		page.Host = host
		pageRec := httptest.NewRecorder()
		h.ServeHTTP(pageRec, page)
		if pageRec.Body.String() != "SAME-BYTES" {
			t.Fatalf("%s served %q, want the shared bundle", host, pageRec.Body.String())
		}

		req := httptest.NewRequest(http.MethodGet, runtimeConfigPath, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var doc struct {
			Settings map[string]string `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: decode: %v", host, err)
		}
		got[host] = doc.Settings["apiBase"]
	}
	if got["eu.example.com"] != "https://api.eu.example" || got["us.example.com"] != "https://api.us.example" {
		t.Errorf("the two deployables did not read their own settings: %v", got)
	}
}

// The document copies the row's map rather than aliasing it: the resolver
// caches the Site, and a document handed the cached map would let a mutation
// of one leak into the other.
func TestSettingsForSite_CopiesRatherThanAliases(t *testing.T) {
	site := &Site{Settings: map[string]string{"a": "1"}}
	doc := settingsForSite(site)
	doc["a"] = "changed"
	if site.Settings["a"] != "1" {
		t.Error("the document's settings alias the cached row's map")
	}
}

// siteFromRow keeps only STRING values, and never yields nil: the guard admits
// nothing but strings, so a number here is a raw write that bypassed it, and a
// bundle reading config.settings.<key> must never get one coerced into a
// string it never was.
func TestSiteFromRow_ProjectsOnlyStringSettings(t *testing.T) {
	row := map[string]any{
		"id":       "v1:platform:site:s1",
		"hostname": "app.example.com",
		"settings": map[string]any{"apiBase": "https://api.example", "retries": 3.0, "debug": true, "nested": map[string]any{"x": "y"}},
	}
	site := siteFromRow(row)
	if len(site.Settings) != 1 || site.Settings["apiBase"] != "https://api.example" {
		t.Errorf("Settings = %v, want only the string entry", site.Settings)
	}
	for name, raw := range map[string]any{"absent": nil, "null": map[string]any{"settings": nil}, "scalar": map[string]any{"settings": "x"}} {
		r := map[string]any{"id": "v1:platform:site:s1"}
		if m, ok := raw.(map[string]any); ok {
			for k, v := range m {
				r[k] = v
			}
		}
		if got := siteFromRow(r).Settings; got == nil || len(got) != 0 {
			t.Errorf("%s: Settings = %v, want an empty, non-nil map", name, got)
		}
	}
}

// THE ACCEPTANCE CRITERION, end to end through the two pieces that carry it:
// a settings write reaches the SERVED document within one invalidation.
//
// The chain has three links and only the middle one is this epic's. The write
// broadcasts `graph.node.updated.v1:platform:site` (a routing rule that
// already existed); SiteInvalidationSubscriber evicts the resolver's cached
// row by hostname (already existed, and needs no change because the payload's
// merged row still carries the hostname); and the NEXT request re-queries and
// serves the new settings, which is what this pins.
//
// Without it the epic's claim rests on inspection: the resolver caches a Site
// for MEMQL_EDGE_SITE_CACHE_TTL_SECONDS, so a settings write with no eviction
// would be invisible for up to thirty seconds and a test that never
// invalidated could not tell the two apart.
func TestSettingsReachTheServedDocumentAfterOneInvalidation(t *testing.T) {
	const host = "app.example.com"
	exec := &stubExec{rows: map[string]*Site{
		host: {ID: "s1", Hostname: host, Status: "live", Kind: "spa", Settings: map[string]string{"apiBase": "https://api.old.example"}},
	}}
	resolver := NewResolver(exec, time.Hour) // a TTL long enough that only an eviction can explain a change
	h := NewHandler(Options{Resolver: resolver, Opener: mapOpener{"index.html": "ROOT"}})

	read := func() string {
		req := httptest.NewRequest(http.MethodGet, runtimeConfigPath, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var doc struct {
			Settings map[string]string `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return doc.Settings["apiBase"]
	}

	if got := read(); got != "https://api.old.example" {
		t.Fatalf("first read = %q, want the stored value", got)
	}

	// The write lands: the row changes, and nothing tells the resolver.
	exec.rows[host] = &Site{ID: "s1", Hostname: host, Status: "live", Kind: "spa", Settings: map[string]string{"apiBase": "https://api.new.example"}}
	if got := read(); got != "https://api.old.example" {
		t.Fatalf("read = %q before any invalidation; the cache is what the eviction exists to beat, so this must still be the OLD value", got)
	}

	// One invalidation -- what the subscriber does on the broadcast.
	resolver.Invalidate(host)
	if got := read(); got != "https://api.new.example" {
		t.Errorf("read = %q after invalidation, want the new value -- a bundle would keep reading the old endpoint", got)
	}
}
