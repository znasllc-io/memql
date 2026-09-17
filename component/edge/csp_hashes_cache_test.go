// component/edge/csp_hashes_cache_test.go
package edge

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// countingFS counts Open calls per name, across every request a countingOpener
// serves. Scanning a document for inline scripts costs a read of that document,
// and the point of the cache is that the read happens once per (bundle
// version, path) rather than once per visitor -- which is a fact about how
// many times the file is opened, so that is what this measures.
type countingFS struct {
	fsys   fs.FS
	mu     *sync.Mutex
	counts map[string]int
}

func (c countingFS) Open(name string) (fs.File, error) {
	c.mu.Lock()
	c.counts[name]++
	c.mu.Unlock()
	return c.fsys.Open(name)
}

type countingOpener struct {
	files  map[string]string
	mu     *sync.Mutex
	counts map[string]int
}

func newCountingOpener(files map[string]string) *countingOpener {
	return &countingOpener{files: files, mu: &sync.Mutex{}, counts: map[string]int{}}
}

func (o *countingOpener) Open(string) (fs.FS, error) {
	mod := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	m := fstest.MapFS{}
	for name, content := range o.files {
		m[name] = &fstest.MapFile{Data: []byte(content), ModTime: mod}
	}
	return countingFS{fsys: m, mu: o.mu, counts: o.counts}, nil
}

func (o *countingOpener) opens(name string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.counts[name]
}

var _ BundleOpener = (*countingOpener)(nil)

// A repeat visitor must not re-scan a document that has not changed. The
// bundle a version prefix names is immutable by construction, so the answer
// cannot have changed -- re-deriving it per request is pure waste, and on the
// largest real document measured (6.34MB, 56 inline scripts) it is ~45ms and
// a 6.34MB read of waste.
func TestDocumentIsScannedOncePerVersionNotOncePerRequest(t *testing.T) {
	opener := newCountingOpener(map[string]string{"index.html": nextish})
	h := NewHandler(Options{Resolver: staticResolver{site: testSite()}, Opener: opener})

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "shop.example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := get()
	if !strings.Contains(scriptSrcOf(t, first), hashAlert1) {
		t.Fatal("first response lacks the hashes -- wrong bug")
	}
	afterFirst := opener.opens("index.html")

	second := get()
	if !strings.Contains(scriptSrcOf(t, second), hashAlert1) {
		t.Error("second response lost the hashes -- the cache must return them, not suppress them")
	}
	afterSecond := opener.opens("index.html")

	// The second request still opens the file to SERVE it; what it must not
	// do is open it a second time to scan it again.
	if extra := afterSecond - afterFirst; extra >= afterFirst {
		t.Errorf("second request opened index.html %d time(s); the first opened it %d. "+
			"The scan was repeated instead of being served from the cache.", extra, afterFirst)
	}
}

// Two different documents in the same bundle must not share a cache entry.
// Keying on the bundle alone would serve one page's hashes on another page,
// which blocks that page's real scripts -- the exact bug, reintroduced by the
// fix.
func TestDifferentDocumentsGetTheirOwnHashes(t *testing.T) {
	other := `<html><body><script>console.log("x")</script></body></html>`
	opener := newCountingOpener(map[string]string{"index.html": nextish, "about.html": other})
	h := NewHandler(Options{Resolver: staticResolver{site: testSite()}, Opener: opener})

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "shop.example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return scriptSrcOf(t, rec)
	}

	root := get("/")
	about := get("/about")

	if !strings.Contains(root, hashAlert1) {
		t.Errorf("/ script-src = %q, want the alert(1) hash", root)
	}
	if !strings.Contains(about, hashConsole) {
		t.Errorf("/about script-src = %q, want the console.log hash", about)
	}
	if strings.Contains(about, hashAlert1) {
		t.Errorf("/about carries /'s hash -- the cache key is not per document: %q", about)
	}
}
