// component/edge/site_publish_serve_test.go -- memql#4345.
//
// The end-to-end claim the deploy-from-the-Library feature makes is not
// "Publish returned a version"; it is "the edge SERVES the new bundle, and
// pointing the row back at the previous version serves the previous bundle
// again". Publisher.Publish and the blob opener are each unit-tested on
// their own terms (publish_test.go, blob_test.go); nothing joined them, so
// nothing checked that a published version is READABLE through the exact
// prefix convention the opener expects. A one-character change to either
// side's prefix would leave both suites green and every deploy 503.
package edge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// memBlobs is one in-memory object store standing in for the container, and
// it deliberately serves BOTH halves: Put comes from the publisher, Get goes
// to the serving path. Two separate fakes would let the test pass with the
// two sides disagreeing about the key -- which is the whole failure this
// file exists to catch.
type memBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemBlobs() *memBlobs { return &memBlobs{objects: map[string][]byte{}} }

func (m *memBlobs) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
	return nil
}

func (m *memBlobs) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (m *memBlobs) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	return out
}

var (
	_ BlobWriter = (*memBlobs)(nil)
	_ BlobClient = (*memBlobs)(nil)
)

// rowSiteStore is the site row: the ONE mutable cell a publish writes and a
// rollback rewrites. Modelling it as a cell rather than a call recorder is
// the point -- rollback is not a different operation, it is this same cell
// set back to a value it already held.
type rowSiteStore struct {
	mu        sync.Mutex
	bundleRef string
}

func (s *rowSiteStore) UpdateBundleRef(_ context.Context, _, bundleRef string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bundleRef = bundleRef
	return nil
}

func (s *rowSiteStore) ref() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bundleRef
}

// serveSite issues one GET against a Handler reading whatever the row
// currently points at, and returns the status + body.
func serveSite(t *testing.T, blobs *memBlobs, site *Site, urlPath string) (int, string) {
	t.Helper()
	h := NewHandler(Options{
		Resolver: staticResolver{site: site},
		Opener:   NewBlobOpener(blobs),
	})
	req := httptest.NewRequest(http.MethodGet, urlPath, nil)
	req.Host = site.Hostname
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestPublishedBundleIsServedAndRollbackRestoresThePrevious is the whole
// deploy story in one test: publish v1, serve it, publish v2, serve THAT,
// then point the row back at v1 and serve v1 again.
//
// The rollback half is what needs the earlier bytes to still exist, which
// is exactly what Publish's version-prefix convention buys and what an
// overwrite-in-place implementation would have destroyed. So the assertion
// is not "the ref changed back" (trivially true), it is "the CONTENT came
// back", which can only hold if v1's objects were never touched by v2's
// publish.
func TestPublishedBundleIsServedAndRollbackRestoresThePrevious(t *testing.T) {
	blobs := newMemBlobs()
	row := &rowSiteStore{}
	pub := NewPublisher(blobs, row)

	site := &Site{
		ID:       "s1",
		Hostname: "shop.example.com",
		Kind:     "spa",
		Status:   "live",
	}

	v1, err := pub.Publish(context.Background(), site.ID, Bundle{
		"index.html":    []byte("<!doctype html><title>v1</title>"),
		"assets/app.js": []byte("console.log('v1')"),
	})
	if err != nil {
		t.Fatalf("publish v1: %v", err)
	}
	site.BundleRef = row.ref()

	if code, body := serveSite(t, blobs, site, "/"); code != http.StatusOK || !strings.Contains(body, "v1") {
		t.Fatalf("serving v1 = %d %q, want 200 carrying v1", code, body)
	}
	if code, body := serveSite(t, blobs, site, "/assets/app.js"); code != http.StatusOK || !strings.Contains(body, "v1") {
		t.Fatalf("serving v1's asset = %d %q, want 200 carrying v1", code, body)
	}

	v2, err := pub.Publish(context.Background(), site.ID, Bundle{
		"index.html":    []byte("<!doctype html><title>v2</title>"),
		"assets/app.js": []byte("console.log('v2')"),
	})
	if err != nil {
		t.Fatalf("publish v2: %v", err)
	}
	if v2.Version == v1.Version {
		t.Fatalf("v2 reused v1's version id %q; different content must produce a different version", v1.Version)
	}
	site.BundleRef = row.ref()
	if site.BundleRef != v2.BundleRef {
		t.Fatalf("the row points at %q after publishing v2, want %q", site.BundleRef, v2.BundleRef)
	}

	if code, body := serveSite(t, blobs, site, "/"); code != http.StatusOK || !strings.Contains(body, "v2") {
		t.Fatalf("serving v2 = %d %q, want 200 carrying v2", code, body)
	}

	// ROLLBACK. Nothing is re-uploaded: the row is set back to the ref it
	// held before, which is the same write updateSiteBundle performs.
	if err := row.UpdateBundleRef(context.Background(), site.ID, v1.BundleRef); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	site.BundleRef = row.ref()

	code, body := serveSite(t, blobs, site, "/")
	if code != http.StatusOK {
		t.Fatalf("serving after rollback = %d, want 200", code)
	}
	if !strings.Contains(body, "v1") {
		t.Errorf("after rollback the edge served %q, want v1's content back", body)
	}
	if code, body := serveSite(t, blobs, site, "/assets/app.js"); code != http.StatusOK || !strings.Contains(body, "v1") {
		t.Errorf("after rollback the asset = %d %q, want v1's asset back", code, body)
	}

	// Both versions' objects coexist under distinct prefixes -- the storage
	// fact rollback depends on.
	var v1Keys, v2Keys int
	for _, k := range blobs.keys() {
		switch {
		case strings.HasPrefix(k, fmt.Sprintf("sites/%s/%s/", site.ID, v1.Version)):
			v1Keys++
		case strings.HasPrefix(k, fmt.Sprintf("sites/%s/%s/", site.ID, v2.Version)):
			v2Keys++
		default:
			t.Errorf("object %q is under neither version's prefix", k)
		}
	}
	if v1Keys != 2 || v2Keys != 2 {
		t.Errorf("object counts = v1:%d v2:%d, want 2 each (both versions fully retained)", v1Keys, v2Keys)
	}
}

// A shopify_storefront bundle is a spa bundle (design D4), so its
// client-side routes must survive a hard reload the same way a spa's do.
// Without the kind in ServeHTTP's fallback, /products/hat 404s.
func TestStorefrontKindGetsTheSPAFallback(t *testing.T) {
	blobs := newMemBlobs()
	row := &rowSiteStore{}
	pub := NewPublisher(blobs, row)

	site := &Site{
		ID:       "s-store",
		Hostname: "shop.acme.example.com",
		Kind:     "shopify_storefront",
		Status:   "live",
	}
	if _, err := pub.Publish(context.Background(), site.ID, Bundle{
		"index.html": []byte("<!doctype html><title>storefront</title>"),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	site.BundleRef = row.ref()

	code, body := serveSite(t, blobs, site, "/products/hat")
	if code != http.StatusOK {
		t.Fatalf("GET /products/hat on a storefront = %d, want 200 (the spa fallback)", code)
	}
	if !strings.Contains(body, "storefront") {
		t.Errorf("the fallback served %q, want index.html", body)
	}

	// The instrument moves: a `static` site with the same bundle 404s the
	// same path, so the assertion above is about the KIND and not about
	// resolveAsset quietly matching everything.
	static := *site
	static.Kind = "static"
	if code, _ := serveSite(t, blobs, &static, "/products/hat"); code != http.StatusNotFound {
		t.Errorf("GET /products/hat on a static site = %d, want 404", code)
	}
}
