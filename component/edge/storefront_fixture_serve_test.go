// component/edge/storefront_fixture_serve_test.go
package edge

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// THE FIXTURE BUNDLE, SERVED THROUGH THE REAL HANDLER (memql#5536).
//
// test/clustere2e/storefront_serving_test.go publishes this same tree to a
// real cluster and serves it through a real edge pod, which is the leg the
// issue asks for -- and which SKIPS on every machine and every CI lane that
// has no cluster. A gate skipped by default cannot be what stands between a
// feature and the bug it prevents (CLAUDE.md, on the worker forward hop), so
// the part that needs no cluster runs here, in the ordinary lane: the actual
// fixture, through the actual handler, under the actual policy.
//
// WHAT THIS COVERS THAT THE UNIT TESTS DO NOT. csp_storefront_test.go asserts
// what policyForSite returns for a Site VALUE. This serves a real directory
// through ServeHTTP and reads the HEADER off the response -- so it also
// covers the wiring between them, the resolution order over a real tree, and
// the fixture's own files being where its document says they are.
//
// WHAT IT DOES NOT COVER, and the clustere2e leg does: the publish path
// (zip -> Library artifact -> sitePublishFromArtifact), object storage, and a
// real pod. Nothing here runs a browser either -- see that leg's header.

// fixtureDir is the bundle the cluster-e2e leg publishes. Reached by relative
// path rather than copied: two copies of a fixture drift, and the one this
// test serves must be the one that cluster leg publishes or this proves
// nothing about it.
const fixtureDir = "../../test/clustere2e/testdata/storefront-fixture"

// dirOpener serves one on-disk directory as a bundle, whatever ref it is
// handed -- the same simplification mapOpener makes, over a real tree.
type dirOpener string

func (d dirOpener) Open(string) (fs.FS, error) { return os.DirFS(string(d)), nil }

func fixtureStorefrontSite() *Site {
	return &Site{
		ID:       "s-fixture",
		Hostname: "fixture.example.com",
		Status:   "live",
		Kind:     storefrontKind,
		Binding: map[string]any{
			"storeDomain":        "fixture-store.myshopify.com",
			"storefrontTokenRef": "fixture_storefront_token",
		},
	}
}

func serveFixture(t *testing.T, site *Site, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(Options{
		Resolver: staticResolver{site: site},
		Opener:   dirOpener(fixtureDir),
		SecretResolver: func(context.Context, string) (string, error) {
			return "fixture-storefront-token-0123456789", nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = site.Hostname
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The bundle is real, and every file its document names is in it. A fixture
// whose stylesheet 404s still renders -- badly, silently -- which is how a
// broken fixture goes unnoticed until somebody opens it.
func TestTheFixtureBundleServesItsOwnAssets(t *testing.T) {
	site := fixtureStorefrontSite()

	root := serveFixture(t, site, "/")
	if root.Code != http.StatusOK {
		t.Fatalf("GET / answered %d, want 200", root.Code)
	}
	body := root.Body.String()
	if !strings.Contains(body, "Storefront path") {
		t.Fatalf("GET / did not serve the fixture document: %.200s", body)
	}
	// A 200 IS NOT EVIDENCE HERE, and finding that out is what this loop is
	// shaped by. This site takes the index.html fallback, so a MISSING
	// stylesheet is served the index document with status 200 -- a test that
	// checked only the code passed with the file deleted. The assertion has
	// to be that the bytes are the file, which for these two is a content
	// type that is not HTML.
	for ref, wantType := range map[string]string{
		"/storefront.css": "text/css",
		"/storefront.js":  "javascript",
	} {
		if !strings.Contains(body, ref) {
			t.Errorf("the fixture document does not reference %s", ref)
		}
		rec := serveFixture(t, site, ref)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s answered %d, want 200", ref, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("GET %s was served Content-Type %q, want %s -- the document names a file "+
				"the bundle does not carry, and the fallback served index.html in its place",
				ref, ct, wantType)
		}
	}
}

// THE POLICY THE FIXTURE IS ACTUALLY SERVED UNDER names its bound store. This
// is G1 asserted at the seam a browser sees: the response header, not a
// function's return value.
func TestTheFixtureIsServedAPolicyNamingItsStore(t *testing.T) {
	rec := serveFixture(t, fixtureStorefrontSite(), "/")
	policy := rec.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("the fixture document was served with no Content-Security-Policy")
	}
	for _, want := range []string{
		"https://fixture-store.myshopify.com",
		"https://cdn.shopify.com",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("the served policy does not name %s: %s", want, policy)
		}
	}
	if strings.Contains(policy, "*") {
		t.Errorf("the served policy carries a wildcard: %s", policy)
	}
}

// THE FIXTURE'S OWN SCRIPT RUNS UNDER script-src 'self'. It is an external
// file for exactly this reason: an inline script would need its hash in the
// policy, and a fixture that quietly depended on the hash machinery would be
// proving something other than what it claims.
func TestTheFixtureNeedsNoInlineScriptAllowance(t *testing.T) {
	rec := serveFixture(t, fixtureStorefrontSite(), "/")
	scriptSrc := directive(rec.Header().Get("Content-Security-Policy"), "script-src")
	if scriptSrc != "script-src 'self'" {
		t.Errorf("the fixture document was served script-src %q -- it should need no allowance "+
			"beyond 'self'", scriptSrc)
	}
	if strings.Contains(rec.Body.String(), "<script>") {
		t.Error("the fixture document carries an inline script")
	}
}

// The document the fixture reads at load carries the block it reads, with the
// store and the RESOLVED token -- and nothing shaped like an Admin token.
func TestTheFixtureIsServedItsStorefrontBlock(t *testing.T) {
	rec := serveFixture(t, fixtureStorefrontSite(), runtimeConfigPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s answered %d", runtimeConfigPath, rec.Code)
	}
	var doc struct {
		Storefront *StorefrontConfig `json:"storefront"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("runtime-config is not JSON: %v", err)
	}
	if doc.Storefront == nil {
		t.Fatal("no storefront block for a shopify_storefront site")
	}
	if doc.Storefront.StoreDomain != "fixture-store.myshopify.com" {
		t.Errorf("storeDomain = %q", doc.Storefront.StoreDomain)
	}
	if doc.Storefront.StorefrontToken == "" {
		t.Error("the storefront token did not resolve")
	}
	if strings.Contains(rec.Body.String(), "shpat_") {
		t.Error("the served document carries something shaped like a Shopify Admin token")
	}
}

// THE RESOLUTION TAIL, OVER A REAL TREE (memql#5535). The same bundle, the
// same missing path, two sites -- and the only difference between them is the
// field.
func TestTheFixtureHonoursTheResolutionTail(t *testing.T) {
	fallback := fixtureStorefrontSite()
	rec := serveFixture(t, fallback, "/products/nothing-here")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Storefront path") {
		t.Errorf("a storefront declaring no tail answered %d for a missing path, want the "+
			"index.html fallback", rec.Code)
	}

	notFound := fixtureStorefrontSite()
	notFound.ResolutionTail = resolutionTailNotFound
	rec = serveFixture(t, notFound, "/products/nothing-here")
	if rec.Code != http.StatusNotFound {
		t.Errorf("a storefront declaring not_found answered %d for a missing path, want 404", rec.Code)
	}
	// The reachable positive: the same site still serves its root, so the 404
	// is the tail rather than a bundle that stopped opening.
	if rec := serveFixture(t, notFound, "/"); rec.Code != http.StatusOK {
		t.Errorf("the not_found-tail site does not serve its root: %d", rec.Code)
	}
}
