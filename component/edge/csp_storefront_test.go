// component/edge/csp_storefront_test.go
package edge

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// cspStorefrontSite is testSite()'s storefront twin: the same hostname, so a
// policy comparison against policyWithoutHashes differs in EXACTLY the
// storefront sources and nothing else.
func cspStorefrontSite() *Site {
	return &Site{
		ID:       "s1",
		Hostname: "shop.example.com",
		Status:   "live",
		Kind:     storefrontKind,
		// THE STORE IS RESOLVED, NOT COPIED (epic memql#5530). The site row's
		// binding names it; Site.Store is what the resolver read through that
		// name, and it is what the policy and the runtime document both read.
		Binding: map[string]any{"storeId": "store-1"},
		Store: &BoundStore{
			ID:                 "store-1",
			Domain:             "acme-dev.myshopify.com",
			StorefrontTokenRef: "acme_storefront_token",
		},
	}
}

// THE BOUND STORE IS NAMED, AND IT COMES FROM THE BINDING.
//
// The whole of G1: a storefront whose policy does not name its own store
// cannot call the Storefront API at all, and the failure is a console
// violation naming an origin nobody configured anywhere.
func TestStorefrontPolicyNamesTheBoundStore(t *testing.T) {
	got := policyForSite(httptest.NewRequest("GET", "/", nil), cspStorefrontSite(), noEnv, "")

	connect := directive(got, "connect-src")
	if !strings.Contains(connect, "https://acme-dev.myshopify.com") {
		t.Errorf("connect-src does not name the bound store: %q", connect)
	}
}

// Shopify's asset hosts reach img-src AND media-src. media-src exists on a
// storefront policy and on no other kind -- every other site falls back to
// default-src 'self', exactly as it always has.
func TestStorefrontPolicyAdmitsShopifyAssetHosts(t *testing.T) {
	got := policyForSite(httptest.NewRequest("GET", "/", nil), cspStorefrontSite(), noEnv, "")

	img := directive(got, "img-src")
	if !strings.Contains(img, "https://cdn.shopify.com") {
		t.Errorf("img-src does not admit Shopify's CDN: %q", img)
	}
	if !strings.Contains(img, "https://acme-dev.myshopify.com") {
		t.Errorf("img-src does not admit the bound store's own origin: %q", img)
	}

	media := directive(got, "media-src")
	if media == "" {
		t.Fatal("a storefront policy has no media-src directive at all")
	}
	if !strings.Contains(media, "https://cdn.shopify.com") {
		t.Errorf("media-src does not admit Shopify's CDN: %q", media)
	}
}

// The Customer Account API's authorization and token endpoints are on
// shopify.com, which is why Shopify's own Hydrogen default names it.
func TestStorefrontPolicyAdmitsTheCustomerAccountApi(t *testing.T) {
	connect := directive(policyForSite(httptest.NewRequest("GET", "/", nil), cspStorefrontSite(), noEnv, ""), "connect-src")
	if !strings.Contains(connect, "https://shopify.com") {
		t.Errorf("connect-src does not admit the Customer Account API host: %q", connect)
	}
}

// SPA AND STATIC ARE BYTE-IDENTICAL TO WHAT THIS CLUSTER ALREADY SERVES.
//
// The twin of TestPolicyForSiteWithNoHashesIsUnchanged the epic asks for:
// that one pins `static`, this one pins BOTH kinds against the same
// hardcoded string, so a storefront arm that leaked into the shared path
// fails here rather than in somebody's browser.
func TestNonStorefrontPoliciesAreUnchangedByTheStorefrontArm(t *testing.T) {
	for _, kind := range []string{"spa", "static"} {
		t.Run(kind, func(t *testing.T) {
			site := testSite()
			site.Kind = kind
			got := policyForSite(httptest.NewRequest("GET", "/", nil), site, noEnv, "")
			if got != policyWithoutHashes {
				t.Errorf("policy for kind %q changed.\n got: %q\nwant: %q", kind, got, policyWithoutHashes)
			}
		})
	}
}

// A BINDING ON A NON-STOREFRONT ROW BUYS NOTHING. Kind is the gate, exactly
// as it is in storefrontForSite -- so a storeDomain written onto an `spa`
// row (by accident, or by someone probing) widens no policy.
func TestABindingOnANonStorefrontRowAdmitsNothing(t *testing.T) {
	site := testSite() // kind: static
	site.Binding = map[string]any{"storeId": "store-1"}
	// RESOLVED AS WELL AS NAMED, so this is not passing merely because the
	// store read was skipped: the policy is handed a Site that HAS a store
	// and must still widen nothing, because kind is the gate.
	site.Store = &BoundStore{ID: "store-1", Domain: "acme-dev.myshopify.com"}

	got := policyForSite(httptest.NewRequest("GET", "/", nil), site, noEnv, "")
	if got != policyWithoutHashes {
		t.Errorf("a binding on a static row widened the policy.\n got: %q\nwant: %q", got, policyWithoutHashes)
	}
}

// A STOREFRONT WITH NO STORE STILL GETS SHOPIFY'S FIXED HOSTS AND NO
// INVENTED ONE. The store-specific origin is the only part the binding
// supplies; an unbound storefront must not produce a policy naming an empty
// or fabricated host.
func TestAnUnboundStorefrontNamesNoStore(t *testing.T) {
	site := cspStorefrontSite()
	site.Binding = nil
	site.Store = nil

	got := policyForSite(httptest.NewRequest("GET", "/", nil), site, noEnv, "")
	connect := directive(got, "connect-src")
	if strings.Contains(connect, "https:// ") || strings.Contains(connect, "https://;") {
		t.Errorf("connect-src carries an empty host: %q", connect)
	}
	if !strings.Contains(connect, "https://cdn.shopify.com") {
		t.Errorf("an unbound storefront lost Shopify's fixed hosts: %q", connect)
	}
}

// A MALFORMED storeDomain IS DROPPED, NOT INTERPOLATED. The value is row
// data, and a row is not literal code: validHost is the same whitelist the
// site's own hostname passes through, for the same reason.
func TestAMalformedStoreDomainIsDropped(t *testing.T) {
	for name, domain := range map[string]string{
		"header injection": "acme.myshopify.com\r\nX-Evil: 1",
		"a space":          "acme dev.myshopify.com",
		"a policy break":   "acme.myshopify.com; script-src *",
		"a scheme":         "https://acme.myshopify.com",
	} {
		t.Run(name, func(t *testing.T) {
			site := cspStorefrontSite()
			site.Store = &BoundStore{ID: "store-1", Domain: domain}

			got := policyForSite(httptest.NewRequest("GET", "/", nil), site, noEnv, "")
			if strings.Contains(got, domain) {
				t.Errorf("a malformed storeDomain reached the header: %q", got)
			}
			if strings.Contains(got, "\r") || strings.Contains(got, "\n") {
				t.Errorf("the policy carries a newline: %q", got)
			}
		})
	}
}

// NEVER A WILDCARD, on any directive, for any site. The issue says so in as
// many words, and a wildcard is the one mistake that makes every other
// assertion here worthless.
func TestNoStorefrontSourceIsAWildcard(t *testing.T) {
	got := policyForSite(httptest.NewRequest("GET", "/", nil), cspStorefrontSite(), noEnv, "")
	for _, d := range strings.Split(got, ";") {
		for _, tok := range strings.Fields(strings.TrimSpace(d)) {
			if strings.Contains(tok, "*") {
				t.Errorf("policy carries a wildcard source %q in %q", tok, strings.TrimSpace(d))
			}
		}
	}
}

// SCRIPT-SRC IS NOT WIDENED. Admitting a third party's script host runs
// somebody else's code in the storefront's own origin, and nothing in the
// binding requires it -- so a storefront's script-src is the same
// `'self'` (+ its own hashes) every other kind gets.
func TestStorefrontScriptSrcIsNotWidened(t *testing.T) {
	got := policyForSite(httptest.NewRequest("GET", "/", nil), cspStorefrontSite(), noEnv, "")
	if scriptSrc := directive(got, "script-src"); scriptSrc != "script-src 'self'" {
		t.Errorf("a storefront's script-src was widened: %q", scriptSrc)
	}
}

// The storefront arm composes with inline-script hashes rather than
// replacing them -- a storefront bundle with an inline bootstrap must get
// both.
func TestStorefrontPolicyStillCarriesInlineScriptHashes(t *testing.T) {
	got := policyForSite(httptest.NewRequest("GET", "/", nil), cspStorefrontSite(), noEnv, hashAlert1)
	if scriptSrc := directive(got, "script-src"); !strings.Contains(scriptSrc, hashAlert1) {
		t.Errorf("the storefront arm dropped the inline-script hashes: %q", scriptSrc)
	}
}
