// component/edge/csp_hashes_serve_test.go
package edge

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// A page shaped like a Next.js static export: an external bundle plus the
// inline hydration payload. The inline body is `alert(1)` so the expected
// hash is the hardcoded vector in scripthash_test.go rather than something
// recomputed here.
const nextish = `<!DOCTYPE html><html><body><div id="root">rendered</div>` +
	`<script src="/_next/static/chunks/app.js"></script>` +
	`<script>alert(1)</script></body></html>`

// datedOpener is mapOpener with a real ModTime, which is what statETag
// needs to emit a validator at all -- fstest.MapFS's zero ModTime is
// refused on purpose (see assetcache.go), so the default test opener can
// never produce a 304 and cannot exercise the revalidation path.
type datedOpener map[string]string

func (m datedOpener) Open(string) (fs.FS, error) {
	mod := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	files := fstest.MapFS{}
	for name, content := range m {
		files[name] = &fstest.MapFile{Data: []byte(content), ModTime: mod}
	}
	return files, nil
}

var _ BundleOpener = datedOpener(nil)

func serveDated(t *testing.T, site *Site, files map[string]string, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: datedOpener(files)})
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "shop.example.com"
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func scriptSrcOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header on the response")
	}
	return directive(csp, "script-src")
}

// The headline behaviour: a served HTML document names its own inline
// scripts, so they execute under an enforcing policy.
func TestHtmlDocumentCarriesItsInlineScriptHashes(t *testing.T) {
	rec := serveDated(t, testSite(), map[string]string{"index.html": nextish}, "/", nil)

	if got := scriptSrcOf(t, rec); !strings.Contains(got, hashAlert1) {
		t.Errorf("script-src does not name the inline script: %q", got)
	}
}

// A document with no inline scripts gets the policy it always got. This is
// the serving-path half of TestPolicyForSiteWithNoHashesIsUnchanged.
func TestHtmlDocumentWithNoInlineScriptsIsUnchanged(t *testing.T) {
	rec := serveDated(t, testSite(), map[string]string{
		"index.html": `<html><body><script src="/app.js"></script></body></html>`}, "/", nil)

	if got := scriptSrcOf(t, rec); got != "script-src 'self'" {
		t.Errorf("script-src = %q, want %q", got, "script-src 'self'")
	}
}

// Hashes describe a DOCUMENT. A script or stylesheet response has no inline
// scripts to name, and naming a document's hashes on an asset response
// would publish them on a resource that never needed them.
func TestNonDocumentResponsesCarryNoHashes(t *testing.T) {
	files := map[string]string{"index.html": nextish, "app.js": "alert(1)", "app.css": "body{}"}
	for _, path := range []string{"/app.js", "/app.css"} {
		t.Run(path, func(t *testing.T) {
			rec := serveDated(t, testSite(), files, path, nil)
			if got := scriptSrcOf(t, rec); got != "script-src 'self'" {
				t.Errorf("script-src on %s = %q, want %q", path, got, "script-src 'self'")
			}
		})
	}
}

// THE SECOND-VISIT REGRESSION TEST.
//
// index.html is served `no-cache`, meaning "revalidate before use", so a
// returning visitor sends If-None-Match and is answered 304. A 304's
// headers REPLACE the stored ones, so a 304 carrying a hash-free policy
// overwrites the good policy the 200 established and the page dies -- on
// the second visit only, having worked perfectly on the first.
//
// Reproduced in a real browser before this was written: first visit
// hydrated, second visit inert, with the injected-script probe still
// blocked in both. It is invisible to a hard reload, which refetches rather
// than revalidating, so the manual test most people would run does not
// find it.
func TestNotModifiedRepeatsTheInlineScriptHashes(t *testing.T) {
	files := map[string]string{"index.html": nextish}

	first := serveDated(t, testSite(), files, "/", nil)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first response -- this test cannot reach the 304 path")
	}
	if !strings.Contains(scriptSrcOf(t, first), hashAlert1) {
		t.Fatal("first response already lacks the hashes -- wrong bug")
	}

	second := serveDated(t, testSite(), files, "/", http.Header{"If-None-Match": {etag}})
	if second.Code != http.StatusNotModified {
		t.Fatalf("second response = %d, want 304 -- this test cannot reach the 304 path", second.Code)
	}
	if got := scriptSrcOf(t, second); !strings.Contains(got, hashAlert1) {
		t.Errorf("304 dropped the inline script hashes, so the page breaks on the second visit.\nscript-src = %q", got)
	}
}

// An spa site falls back to index.html for an unknown path, and that
// fallback is a DOCUMENT -- it needs its hashes exactly as the root does.
// Without this, every client-side route breaks on a hard reload while the
// root works.
func TestSpaFallbackCarriesTheInlineScriptHashes(t *testing.T) {
	site := &Site{ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa"}
	rec := serveDated(t, site, map[string]string{"index.html": nextish}, "/some/client/route", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("spa fallback = %d, want 200", rec.Code)
	}
	if got := scriptSrcOf(t, rec); !strings.Contains(got, hashAlert1) {
		t.Errorf("spa fallback dropped the hashes: %q", got)
	}
}

// A static site 404s rather than falling back. The response still carries
// the base policy -- a 404 body is still a document the browser renders.
func TestNotFoundStillCarriesTheBasePolicy(t *testing.T) {
	rec := serveDated(t, testSite(), map[string]string{"index.html": nextish}, "/nope", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("static miss = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("404 response carries no Content-Security-Policy at all")
	}
}

// Resolution reaches a document through three names: the exact file,
// <path>/index.html and <path>.html. All three are documents.
func TestEveryDocumentResolutionRungCarriesHashes(t *testing.T) {
	files := map[string]string{
		"index.html":      nextish,
		"about.html":      nextish,
		"deep/index.html": nextish,
		"exact/page.html": nextish,
	}
	for _, path := range []string{"/", "/about", "/deep", "/exact/page.html"} {
		t.Run(path, func(t *testing.T) {
			rec := serveDated(t, testSite(), files, path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200", path, rec.Code)
			}
			if got := scriptSrcOf(t, rec); !strings.Contains(got, hashAlert1) {
				t.Errorf("%s dropped the hashes: %q", path, got)
			}
		})
	}
}
