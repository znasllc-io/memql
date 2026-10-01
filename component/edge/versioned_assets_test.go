package edge

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func publishedFixture(t *testing.T, bundle Bundle) (*Site, *Handler) {
	t.Helper()
	site := &Site{ID: "shop", Hostname: "shop.example.test", Status: "live", Kind: storefrontKind,
		BundleRef: "blob://sites/shop/" + version(bundle) + "/"}
	objects := map[string][]byte{}
	for name, body := range bundle {
		objects[strings.TrimPrefix(site.BundleRef, "blob://")+name] = body
	}
	return site, NewHandler(Options{Resolver: staticResolver{site}, Opener: NewBlobOpener(newCountingBlobClient(objects))})
}

func TestVersionedAssetsKeepImportsAndContentStableAcrossReleases(t *testing.T) {
	inline := "\r\nconsole.log('unchanged & literal')\r\n"
	bundle := Bundle{
		"index.html":                []byte(`<html><head><link rel="stylesheet" href="/_astro/style.BUujHlVu.css?theme=one&amp;v=1#sheet"><script type="module" src="/_astro/app.CpY0cJgo.js"></script><script>` + inline + `</script></head></html>`),
		"_astro/app.CpY0cJgo.js":    []byte(`import "./chunk.DTqiKqWR.js";`),
		"_astro/chunk.DTqiKqWR.js":  []byte(`export const value = 1;`),
		"_astro/style.BUujHlVu.css": []byte(`body { color: red }`),
	}
	site, h := publishedFixture(t, bundle)
	prefix := assetPrefixFor(site)
	page := get(t, h, "/", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), `src="`+prefix+`_astro/app.CpY0cJgo.js"`) || !strings.Contains(page.Body.String(), `href="`+prefix+`_astro/style.BUujHlVu.css?theme=one&amp;v=1#sheet"`) || !strings.Contains(page.Body.String(), inline) {
		t.Fatalf("asset references or inline bytes changed incorrectly: %d %s", page.Code, page.Body.String())
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "'sha256-") {
		t.Fatal("inline script hashes missing")
	}
	if got := get(t, h, "/_astro/app.CpY0cJgo.js", nil).Header().Get("Cache-Control"); !isNoCache(got) {
		t.Fatalf("unverified filename became immutable: %q", got)
	}
	assetURL, _ := url.Parse(prefix + "_astro/app.CpY0cJgo.js")
	chunk, _ := url.Parse("./chunk.DTqiKqWR.js")
	for _, asset := range []string{assetURL.String(), assetURL.ResolveReference(chunk).String()} {
		first := get(t, h, asset, nil)
		if first.Code != 200 || first.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("versioned asset: %d %v", first.Code, first.Header())
		}
		if next := get(t, h, asset, map[string]string{"If-None-Match": first.Header().Get("ETag")}); next.Code != 304 || next.Header().Get("Cache-Control") != first.Header().Get("Cache-Control") {
			t.Fatalf("conditional asset: %d %v", next.Code, next.Header())
		}
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodHead, asset, nil))
		if r.Code != 200 || r.Body.Len() != 0 || r.Header().Get("ETag") != first.Header().Get("ETag") {
			t.Fatalf("HEAD: %d %v %q", r.Code, r.Header(), r.Body.String())
		}
	}
	rangeReply := get(t, h, assetURL.String(), map[string]string{"Range": "bytes=0-5"})
	if rangeReply.Code != 206 || rangeReply.Body.String() != "import" {
		t.Fatalf("range: %d %q", rangeReply.Code, rangeReply.Body.String())
	}
	// A new deployment may reuse a short bundler filename. Its URL must
	// change, and the old URL must never return these different bytes.
	bundle["_astro/chunk.DTqiKqWR.js"] = []byte(`export const value = 2;`)
	newSite, newer := publishedFixture(t, bundle)
	if assetPrefixFor(newSite) == prefix {
		t.Fatal("changed bundle kept its asset URL")
	}
	old := get(t, newer, assetURL.String(), nil)
	if old.Code != 404 || !strings.Contains(old.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("old version did not refuse: %d %v", old.Code, old.Header())
	}
	nextPage := get(t, newer, "/", map[string]string{"If-None-Match": page.Header().Get("ETag")})
	if nextPage.Code != 200 || nextPage.Header().Get("ETag") == page.Header().Get("ETag") {
		t.Fatal("new asset URLs hidden behind stale HTML validator")
	}
}

func TestVersionedAssetsDoNotBypassPreviewOrSiteAccess(t *testing.T) {
	public := "blob://sites/shop/v111111111111/"
	private := "blob://sites/shop/v222222222222/"
	site := &Site{ID: "shop", Hostname: "shop.example.test", Status: "live", Kind: "spa", BundleRef: public, CandidateRef: private}
	exec := newStubPreviewExec()
	token := exec.issue(t, PreviewGrant{ID: "grant", SiteID: "shop", CandidateRef: private})
	h := NewHandler(Options{Resolver: staticResolver{site}, PreviewExec: exec, Opener: refOpener{
		public: {"_astro/app.js": "public"}, private: {"_astro/app.js": "private"},
	}})
	publicURL := assetPrefixFor(site) + "_astro/app.js"
	privateURL := assetPrefixFor(&Site{BundleRef: private}) + "_astro/app.js"
	if got := get(t, h, privateURL, nil); got.Code != 404 {
		t.Fatalf("candidate leaked without grant: %d %q", got.Code, got.Body.String())
	}
	req := httptest.NewRequest("GET", privateURL, nil)
	req.AddCookie(&http.Cookie{Name: memql.PreviewCookieName, Value: token})
	result := httptest.NewRecorder()
	h.ServeHTTP(result, req)
	if result.Code != 200 || result.Body.String() != "private" || !strings.Contains(result.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("authorized preview: %d %v %q", result.Code, result.Header(), result.Body.String())
	}
	for _, status := range []string{"draft", "disabled", "archived"} {
		site.Status = status
		if got := get(t, h, publicURL, nil); got.Code == 200 {
			t.Fatalf("%s asset remained accessible", status)
		}
	}
	site.Status = "live"
	site.StorefrontTesting = true
	if got := get(t, h, publicURL, nil); got.Code != http.StatusUnauthorized {
		t.Fatalf("Testing asset bypassed its session: %d", got.Code)
	}
	site.StorefrontTesting = false
	site.BundleRef = "blob://sites/other/v111111111111/"
	if got := get(t, h, publicURL, nil); got.Code != 404 {
		t.Fatalf("other site's token matched: %d", got.Code)
	}
}

func TestVersionedAssetsRejectPathsAndDocumentsWithoutFallback(t *testing.T) {
	site, h := publishedFixture(t, Bundle{"index.html": []byte("HOME"), "_astro/a.js": []byte("ASSET")})
	prefix := assetPrefixFor(site)
	for _, name := range []string{"index.html", "_astro/", "_astro/missing.js", "_astro/../a.js", "_astro/%2e%2e/a.js", "_astro/unknown", "//_astro/a.js"} {
		got := get(t, h, prefix+name, nil)
		if got.Code != 404 || !strings.Contains(got.Header().Get("Cache-Control"), "no-store") || strings.Contains(got.Body.String(), "HOME") {
			t.Errorf("%s: %d %v %q", name, got.Code, got.Header(), got.Body.String())
		}
	}
	for _, ref := range []string{"file:///app/site", "blob://sites/shop/current/", "blob://other/prefix/"} {
		if assetPrefixFor(&Site{BundleRef: ref}) != "" {
			t.Fatalf("mutable layout gained immutable URL: %s", ref)
		}
	}
	got := httptest.NewRecorder()
	h.ServeHTTP(got, httptest.NewRequest(http.MethodPost, prefix+"_astro/a.js", nil))
	if got.Code != 405 || got.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("versioned assets accepted a write")
	}
}

func TestMissingFilesNeverBecomeTheHomePage(t *testing.T) {
	site, h := publishedFixture(t, Bundle{"index.html": []byte("HOME"), "actual.json": []byte(`{"ok":true}`)})
	for _, kind := range []string{"spa", storefrontKind, "static"} {
		site.Kind = kind
		for _, missing := range []string{"/favicon.ico", "/apple-touch-icon.png", "/manifest.webmanifest", "/manifest.json", "/sitemap_index.xml", "/humans.txt", "/_astro/missing.js", "/_astro/missing"} {
			for _, method := range []string{"GET", "HEAD"} {
				got := httptest.NewRecorder()
				h.ServeHTTP(got, httptest.NewRequest(method, missing, nil))
				if got.Code != 404 || !strings.Contains(got.Header().Get("Cache-Control"), "no-store") || strings.Contains(got.Body.String(), "HOME") {
					t.Errorf("%s %s %s: %d %v", kind, method, missing, got.Code, got.Header())
				}
			}
		}
	}
	site.Kind = storefrontKind
	for _, route := range []string{"/products/new-product", "/people/jane.doe"} {
		if got := get(t, h, route, nil); got.Code != 200 || !strings.Contains(got.Body.String(), "HOME") {
			t.Fatalf("client route lost fallback: %s %d", route, got.Code)
		}
	}
	if got := get(t, h, "/actual.json", nil); got.Code != 200 || got.Body.String() != `{"ok":true}` {
		t.Fatal("existing static file no longer resolves")
	}
	for _, malformed := range []string{"/%00", "/%E0%A4", "/%5csecret"} {
		if got := get(t, h, malformed, nil); got.Code != 400 || strings.Contains(got.Body.String(), "HOME") {
			t.Fatalf("malformed path reached bundle: %s %d", malformed, got.Code)
		}
	}
}

func TestAssetLinkRewriteDoesNotTouchInlineCodeOrOtherOrigins(t *testing.T) {
	data := []byte(`<script>let url="/_astro/code.js";</script><style>/* /_astro/style.css */</style><script src="https://cdn.example/_astro/a.js"></script><link href="//cdn.example/_astro/b.css"><script src="/_astro/../outside.js"></script>`)
	if got := versionAssetLinks(data, "/_memql/assets/version/"); string(got) != string(data) {
		t.Fatalf("rewrote non-asset references: %s", got)
	}
}
