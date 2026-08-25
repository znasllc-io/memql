// component/edge/assetcache_test.go
package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// EDGE ASSET CACHING (memql#4545).
//
// Every test here counts DOWNLOADS. That is the measurement the task is
// about: the header policy was already correct before this work, and what
// was missing underneath it was invisible from the headers alone -- an
// `immutable` response that re-downloaded from Azure on every request looks
// identical to one that did not.

// countingBlobClient is a BlobClient that records every Get, so a test can
// assert on how many times the edge actually reached storage.
type countingBlobClient struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    map[string]int
	total   int
	// block, when non-nil, holds every Get until it is closed. Used to
	// force genuine concurrency for the singleflight assertion.
	block chan struct{}
}

func newCountingBlobClient(objects map[string][]byte) *countingBlobClient {
	return &countingBlobClient{objects: objects, gets: map[string]int{}}
}

func (c *countingBlobClient) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	c.gets[key]++
	c.total++
	blocker := c.block
	c.mu.Unlock()

	if blocker != nil {
		<-blocker
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (c *countingBlobClient) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets[key]
}

func (c *countingBlobClient) totalGets() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

const testBundlePrefix = "sites/shop/vab12cd34ef56/"

func blobSiteHandler(t *testing.T, client BlobClient) *Handler {
	t.Helper()
	return NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID:        "shop",
			Hostname:  "shop.example.test",
			Status:    "live",
			Kind:      "spa",
			BundleRef: "blob://" + testBundlePrefix,
		}},
		Opener: NewBlobOpener(client),
	})
}

func get(t *testing.T, h *Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://shop.example.test"+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestConditionalRequestAnswers304WithZeroDownloads is the headline.
//
// Before memql#4545 this was IMPOSSIBLE rather than merely absent: nothing
// set an ETag, and the blob path's ModTime was the zero time, so
// http.ServeContent could never emit a validator for the client to send
// back. A 304 costing zero storage reads is the thing that makes the edge
// affordable in front of a busy site.
func TestConditionalRequestAnswers304WithZeroDownloads(t *testing.T) {
	client := newCountingBlobClient(map[string][]byte{
		testBundlePrefix + "assets/app.abc123.js": []byte("console.log(1)"),
	})
	h := blobSiteHandler(t, client)

	first := get(t, h, "/assets/app.abc123.js", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the first response -- without a validator the client has nothing to send back and a 304 is impossible")
	}
	if etag[0] == 'W' {
		t.Errorf("ETag %q is weak; a strong tag is required for range requests to be satisfiable from it", etag)
	}
	downloadsAfterFirst := client.totalGets()

	second := get(t, h, "/assets/app.abc123.js", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional request: status %d, want 304", second.Code)
	}
	if got := client.totalGets(); got != downloadsAfterFirst {
		t.Errorf("the 304 cost %d extra storage read(s); it must cost zero -- the validator is derived from the content-addressed prefix precisely so the file need not be opened",
			got-downloadsAfterFirst)
	}
	// The 304 repeats the freshness policy. Omitting it leaves the client
	// to a default, which for an immutable asset is the difference between
	// one request a year and one per page load.
	if got := second.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("304 Cache-Control = %q, want the same policy the 200 carried", got)
	}
	if got := second.Header().Get("ETag"); got != etag {
		t.Errorf("304 ETag = %q, want %q", got, etag)
	}
}

// TestIndexHtmlCarriesAValidatorToo. index.html is `no-cache`, which does
// NOT mean "do not store" -- it means "revalidate before use". So a
// returning visitor asks on every load, and before this the answer was
// always the whole document again. This is where 304 pays daily.
func TestIndexHtmlCarriesAValidatorToo(t *testing.T) {
	client := newCountingBlobClient(map[string][]byte{
		testBundlePrefix + "index.html": []byte("<!doctype html><title>shop</title>"),
	})
	h := blobSiteHandler(t, client)

	first := get(t, h, "/", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("index.html carries no ETag -- the no-cache document re-transfers in full on every load, which is every returning visitor")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
		t.Errorf("index.html Cache-Control = %q; the no-cache policy must not change", got)
	}

	before := client.totalGets()
	second := get(t, h, "/", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional index.html: status %d, want 304", second.Code)
	}
	if got := client.totalGets(); got != before {
		t.Errorf("the index.html 304 cost %d extra storage read(s), want 0", got-before)
	}
	if got := second.Header().Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
		t.Errorf("304 Cache-Control = %q, want the no-cache policy repeated", got)
	}
}

// TestRepeatDownloadIsServedFromTheCache. An unconditional second request
// -- a different visitor, a cold browser cache -- must not re-reach storage
// for bytes that are immutable by construction.
func TestRepeatDownloadIsServedFromTheCache(t *testing.T) {
	key := testBundlePrefix + "assets/app.abc123.js"
	client := newCountingBlobClient(map[string][]byte{key: []byte("console.log(1)")})
	h := blobSiteHandler(t, client)

	for i := 0; i < 5; i++ {
		if rec := get(t, h, "/assets/app.abc123.js", nil); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
	}
	if got := client.count(key); got != 1 {
		t.Errorf("%d download(s) for 5 requests of an immutable asset, want 1 -- every visitor was a fresh blob download", got)
	}
}

// TestRepublishFetchesFresh is the invalidation story, and the point is
// that there ISN'T one. A republish lands under a new content-addressed
// prefix, so it is a new cache key; old entries age out by LRU and no purge
// path exists to get wrong.
func TestRepublishFetchesFresh(t *testing.T) {
	oldKey := testBundlePrefix + "index.html"
	const newPrefix = "sites/shop/vffffffffffff/"
	newKey := newPrefix + "index.html"

	client := newCountingBlobClient(map[string][]byte{
		oldKey: []byte("<!doctype html>v1"),
		newKey: []byte("<!doctype html>v2"),
	})
	opener := NewBlobOpener(client)

	serve := func(ref string) string {
		h := NewHandler(Options{
			Resolver: staticResolver{site: &Site{
				ID: "shop", Hostname: "shop.example.test", Status: "live",
				Kind: "spa", BundleRef: ref,
			}},
			Opener: opener,
		})
		return get(t, h, "/", nil).Body.String()
	}

	if got := serve("blob://" + testBundlePrefix); got != "<!doctype html>v1" {
		t.Fatalf("first version served %q", got)
	}
	if got := serve("blob://" + newPrefix); got != "<!doctype html>v2" {
		t.Fatalf("after republish the edge served %q -- the cache survived a version flip, which is a site stuck on old bytes with no way to clear it", got)
	}
	// Both were fetched: the new prefix is a new key, so the cache could
	// not have answered it.
	if got := client.count(newKey); got != 1 {
		t.Errorf("the republished bundle was fetched %d time(s), want 1", got)
	}
}

// TestTheValidatorChangesWithTheVersion. Two bundles must never share a
// validator for the same path, or a visitor holding the old one is told
// "not modified" about bytes that were replaced.
func TestTheValidatorChangesWithTheVersion(t *testing.T) {
	a := &blobFS{prefix: "sites/shop/vaaaaaaaaaaaa/"}
	b := &blobFS{prefix: "sites/shop/vbbbbbbbbbbbb/"}

	etagA, okA := a.ETagFor("index.html")
	etagB, okB := b.ETagFor("index.html")
	if !okA || !okB {
		t.Fatal("a blob bundle could not name its own validator")
	}
	if etagA == etagB {
		t.Fatal("two bundle versions produced the same ETag for index.html -- a visitor holding the old one would be told 'not modified' about bytes that changed")
	}

	// And two different paths in ONE bundle differ, which is what the NUL
	// separator in strongETag is for: without it ("ab","c") and ("a","bc")
	// collide.
	one, _ := a.ETagFor("ab/c.js")
	two, _ := a.ETagFor("a/bc.js")
	if one == two {
		t.Error("two paths collided on one validator -- the component separator is missing or ineffective")
	}
}

// TestConcurrentColdRequestsCollapseToOneDownload pins the singleflight.
// Without it, N concurrent first-requests for one uncached asset each drive
// their own download before any can populate the cache -- and a burst is
// exactly how someone would exercise it.
func TestConcurrentColdRequestsCollapseToOneDownload(t *testing.T) {
	key := testBundlePrefix + "assets/app.abc123.js"
	client := newCountingBlobClient(map[string][]byte{key: []byte("console.log(1)")})
	client.block = make(chan struct{})
	h := blobSiteHandler(t, client)

	const callers = 8
	var wg sync.WaitGroup
	started := make(chan struct{}, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			get(t, h, "/assets/app.abc123.js", nil)
		}()
	}
	for i := 0; i < callers; i++ {
		<-started
	}
	// Every caller is now in flight or about to be; releasing lets the one
	// in-flight download finish and the rest join its result.
	close(client.block)
	wg.Wait()

	if got := client.count(key); got > 2 {
		t.Errorf("%d concurrent downloads for one asset, want the singleflight to collapse them (allowing 1 slip for scheduling)", got)
	}
}

// TestTheFileBundlePathIsUnvalidatedOnlyWhenItCannotStat.
//
// The file:// path stays UNCACHED -- local disk is not what anyone is
// trying to avoid, and this is recorded as a decision rather than an
// omission. It still gets a validator, from what a Stat already returned.
func TestFileBundleGetsAStatDerivedValidator(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<!doctype html>hello")

	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "shop", Hostname: "shop.example.test", Status: "live",
			Kind: "spa", BundleRef: "file://" + dir,
		}},
		Opener: NewFileOpener(),
	})

	first := get(t, h, "/", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the file:// path served no ETag -- a Stat already returns size and mtime, so a validator costs nothing extra")
	}

	second := get(t, h, "/", map[string]string{"If-None-Match": etag})
	if second.Code != http.StatusNotModified {
		t.Fatalf("conditional request on the file path: status %d, want 304", second.Code)
	}
}

// A 404 must not inherit the immutable policy set for the body that is not
// being sent -- that is a browser caching the ABSENCE of a file for a year,
// which survives the deploy that adds it.
func TestAMissDoesNotInheritTheImmutablePolicy(t *testing.T) {
	client := newCountingBlobClient(map[string][]byte{})
	h := blobSiteHandler(t, client)

	rec := get(t, h, "/assets/missing.abc123.js", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
		t.Errorf("404 Cache-Control = %q, want the no-cache policy -- an immutable 404 is cached for a year and survives the deploy that adds the file", got)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("404 carried ETag %q; it describes a body that was not sent", got)
	}
}

// ---- the LRU itself ----------------------------------------------------

func TestBundleCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newBundleCache(30)
	c.Put("a", make([]byte, 10))
	c.Put("b", make([]byte, 10))
	c.Put("c", make([]byte, 10))

	// Touch "a" so "b" becomes the least recently used.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was evicted early")
	}
	c.Put("d", make([]byte, 10))

	if _, ok := c.Get("b"); ok {
		t.Error("b survived; it was the least recently used when d arrived")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s was evicted", k)
		}
	}
	if _, used := c.stats(); used > 30 {
		t.Errorf("cache holds %d bytes, over its %d cap", used, 30)
	}
}

// An entry larger than the whole cache is not stored and evicts nothing.
// Admitting it would flush every other entry to hold one item that the next
// request for anything else immediately evicts.
func TestBundleCacheRefusesAnOversizedEntryWithoutFlushing(t *testing.T) {
	c := newBundleCache(20)
	c.Put("small", make([]byte, 10))
	c.Put("huge", make([]byte, 100))

	if _, ok := c.Get("huge"); ok {
		t.Error("an entry larger than the whole cache was stored")
	}
	if _, ok := c.Get("small"); !ok {
		t.Error("the oversized entry flushed the cache on its way to not being stored")
	}
}

// A disabled cache is a correct cache. Zero or negative means OFF rather
// than unlimited -- the worst outcome of off is the pre-memql#4545
// behaviour, and the worst outcome of unlimited is a dead replica.
func TestZeroSizedCacheIsDisabledNotUnlimited(t *testing.T) {
	c := newBundleCache(0)
	c.Put("a", make([]byte, 10))
	if _, ok := c.Get("a"); ok {
		t.Error("a zero-sized cache stored an entry")
	}

	t.Setenv(bundleCacheMBEnv, "0")
	if got := bundleCacheBytes(); got != 0 {
		t.Errorf("bundleCacheBytes() = %d for MB=0, want 0 (disabled)", got)
	}
	t.Setenv(bundleCacheMBEnv, "not a number")
	if got := bundleCacheBytes(); got != 0 {
		t.Errorf("bundleCacheBytes() = %d for an unparseable value, want 0 (disabled)", got)
	}
	t.Setenv(bundleCacheMBEnv, "")
	if got, want := bundleCacheBytes(), int64(defaultBundleCacheMB)<<20; got != want {
		t.Errorf("bundleCacheBytes() = %d unset, want the %d MB default", got, defaultBundleCacheMB)
	}
	t.Setenv(bundleCacheMBEnv, "8")
	if got, want := bundleCacheBytes(), int64(8)<<20; got != want {
		t.Errorf("bundleCacheBytes() = %d for MB=8, want %d", got, want)
	}
}

func TestETagMatchesHandlesTheHeaderGrammar(t *testing.T) {
	const tag = `"abc123"`
	for _, header := range []string{tag, `"other", "abc123"`, `W/"abc123"`, "*", ` "abc123" `} {
		if !etagMatches(header, tag) {
			t.Errorf("If-None-Match: %s did not match %s", header, tag)
		}
	}
	for _, header := range []string{"", `"nope"`, `"abc12"`} {
		if etagMatches(header, tag) {
			t.Errorf("If-None-Match: %q matched %s and must not", header, tag)
		}
	}
	if etagMatches("*", "") {
		t.Error(`"*" matched an empty tag; a response with no validator has nothing to match`)
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// THE VARY AUDIT (memql#4545), recorded as a test because the conclusion
// is "none is required" and a conclusion of that shape is indistinguishable
// from nobody having looked.
//
// What actually varies an edge response:
//
//   - the request HOST, which selects the site and therefore the bundle, so
//     the same path returns different bytes on two hostnames;
//   - the request SCHEME (r.TLS), which csp.go folds into the per-site
//     origin.
//
// Both are components of the request's effective target URI, and RFC 9111
// makes the target URI part of every cache key -- a shared cache cannot
// collide two hostnames without already being broken for every origin it
// serves. So a `Vary: Host` would restate the cache key rather than
// constrain it.
//
// Nothing varies on a request header a cache would NOT key on: no
// content negotiation, no Accept-Encoding branch (compression is the
// proxy's), no cookie-dependent body. runtime-config.json is the closest
// thing to a per-host document and it is `no-store`, so no cache holds it
// at all.
//
// The cost of getting this wrong in the other direction is why it is a
// test: a blanket `Vary: *` or `Vary: Accept-Encoding, User-Agent` added
// "to be safe" fragments every CDN entry and quietly destroys the hit
// ratio the immutable policy exists to earn.
func TestEdgeResponsesCarryNoBlanketVary(t *testing.T) {
	client := newCountingBlobClient(map[string][]byte{
		testBundlePrefix + "index.html":           []byte("<!doctype html>"),
		testBundlePrefix + "assets/app.abc123.js": []byte("console.log(1)"),
	})
	h := blobSiteHandler(t, client)

	for _, path := range []string{"/", "/assets/app.abc123.js", "/runtime-config.json"} {
		rec := get(t, h, path, nil)
		if got := rec.Header().Get("Vary"); got != "" {
			t.Errorf("%s carries Vary: %q. Host and scheme are already cache-key components (RFC 9111), so a Vary here restates the key and fragments every CDN entry that would otherwise be shared.", path, got)
		}
	}

	// The reachable positive for the claim that runtime-config.json needs
	// no Vary: it is no-store, so nothing caches it in the first place.
	rec := get(t, h, "/runtime-config.json", nil)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("runtime-config.json Cache-Control = %q, want no-store -- the per-host document's safety depends on nothing caching it", got)
	}
}
