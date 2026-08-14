// component/edge/handler_test.go
package edge

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// staticResolver is a Resolver that always answers with the same site (or a
// miss, when site is nil) -- the test double for a real caching Resolver,
// which resolve_test.go already covers on its own terms.
type staticResolver struct {
	site *Site
}

func (s staticResolver) Resolve(context.Context, string) (*Site, error) { return s.site, nil }
func (s staticResolver) Invalidate(string)                              {}

var _ Resolver = staticResolver{}

// mapOpener is a BundleOpener over an in-memory bundle -- the test's stand-in
// for a real file:// or blob:// bundle. It ignores the ref it is given: these
// tests only ever exercise one site's bundle at a time, and the resolution
// order under test lives entirely in what files are present, not in how they
// were opened (bundle_test.go and blob_test.go already cover the openers
// themselves).
type mapOpener map[string]string

func (m mapOpener) Open(string) (fs.FS, error) {
	files := fstest.MapFS{}
	for name, content := range m {
		files[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return files, nil
}

var _ BundleOpener = mapOpener(nil)

// containsOrigin reports whether csp names origin as one of its sources.
// policyForSite emits space-separated directive tokens, so a substring match
// is enough for a test assertion without parsing full CSP grammar.
func containsOrigin(csp, origin string) bool {
	return strings.Contains(csp, origin)
}

// isNoCache reports whether a Cache-Control value is the no-store shape
// noCache() writes ("no-cache, no-store, must-revalidate"), as opposed to the
// long-lived "public, max-age=..., immutable" policy fingerprinted assets get.
func isNoCache(cacheControl string) bool {
	return strings.Contains(cacheControl, "no-store") || strings.Contains(cacheControl, "no-cache")
}

func serve(t *testing.T, site *Site, files map[string]string, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(Options{
		Resolver: staticResolver{site: site},
		Opener:   mapOpener(files),
	})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// D11's resolution order, one case per rung. A prerendered route is a real
// file on disk (products/shoe.html), which is why the .html rung exists at
// all -- without it, prerendering buys nothing because the SPA fallback would
// serve index.html for every route.
func TestResolutionOrder(t *testing.T) {
	files := map[string]string{
		"index.html":         "ROOT",
		"about/index.html":   "ABOUT-DIR",
		"products/shoe.html": "SHOE-PRERENDERED",
		"assets/app.js":      "JS",
	}
	live := &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}

	for _, tc := range []struct{ path, want string }{
		{"/assets/app.js", "JS"},               // exact file
		{"/about", "ABOUT-DIR"},                // <path>/index.html
		{"/products/shoe", "SHOE-PRERENDERED"}, // <path>.html
		{"/cart/anything", "ROOT"},             // spa fallback
	} {
		rec := serve(t, live, files, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, rec.Code)
			continue
		}
		if rec.Body.String() != tc.want {
			t.Errorf("GET %s served %q, want %q", tc.path, rec.Body.String(), tc.want)
		}
	}
}

// A static site 404s an unknown path instead of falling back. A multi-page
// site that silently renders its home page for every typo hides broken links
// from the people who could fix them.
func TestStaticKindDoesNotFallBack(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "static"},
		map[string]string{"index.html": "ROOT"},
		"/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("static site GET /nope = %d, want 404", rec.Code)
	}
}

func TestUnknownHostnameIs404(t *testing.T) {
	rec := serve(t, nil, map[string]string{}, "/")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown hostname = %d, want 404", rec.Code)
	}
}

func TestDraftSiteIs404(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "draft", Kind: "spa"},
		map[string]string{"index.html": "ROOT"}, "/")
	if rec.Code != http.StatusNotFound {
		t.Errorf("draft site = %d, want 404", rec.Code)
	}
}

// 503, NOT 404. A deliberately paused site and a typo'd hostname are
// different situations and the operator debugging one needs to tell them
// apart.
func TestDisabledSiteIs503(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "disabled", Kind: "spa"},
		map[string]string{"index.html": "ROOT"}, "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("disabled site = %d, want 503", rec.Code)
	}
}

// The CSP names the SITE's own origin, not the identity base URL the portal
// handler uses today. A shared policy across every hosted site would be
// either uselessly permissive or wrong for most of them.
func TestCSPNamesTheSiteOrigin(t *testing.T) {
	rec := serve(t,
		&Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"},
		map[string]string{"index.html": "ROOT"}, "/")
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header")
	}
	if !containsOrigin(csp, "https://shop.example.com") {
		t.Errorf("CSP does not name the site origin: %q", csp)
	}
}

// index.html must never be cached: it is how a deploy reaches a returning
// visitor. Fingerprinted assets may be cached hard.
func TestIndexIsNotCachedButAssetsAre(t *testing.T) {
	files := map[string]string{"index.html": "ROOT", "assets/app.js": "JS"}
	live := &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}

	if cc := serve(t, live, files, "/").Header().Get("Cache-Control"); !isNoCache(cc) {
		t.Errorf("index.html Cache-Control = %q, want no-cache", cc)
	}
	if cc := serve(t, live, files, "/assets/app.js").Header().Get("Cache-Control"); isNoCache(cc) {
		t.Errorf("asset Cache-Control = %q, want a cacheable policy", cc)
	}
}
