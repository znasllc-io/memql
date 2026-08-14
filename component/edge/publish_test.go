// component/edge/publish_test.go
package edge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// fakeBlobStore is a BlobWriter that records every Put and can be told to
// start failing partway through a bundle -- the shape TestFailedUpload...
// needs to prove a failure mid-upload never reaches the row flip.
type fakeBlobStore struct {
	failAfter int // 0 means never fail; N means the (N+1)th Put call fails
	calls     int
	objects   map[string][]byte
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{objects: make(map[string][]byte)}
}

func (f *fakeBlobStore) Put(_ context.Context, key string, data []byte) error {
	f.calls++
	if f.failAfter > 0 && f.calls > f.failAfter {
		return fmt.Errorf("fakeBlobStore: simulated failure on call %d (failAfter=%d)", f.calls, f.failAfter)
	}
	f.objects[key] = data
	return nil
}

// hasPrefix reports whether any stored object's key starts with ref's
// prefix (ref is a full "blob://..." BundleRef). Objects are never removed
// by fakeBlobStore -- there is no cleanup path, mirroring Publish itself --
// so this answers "are the bytes still there" the same way blobFS.Open
// would against the real backend.
func (f *fakeBlobStore) hasPrefix(ref string) bool {
	prefix := strings.TrimPrefix(ref, "blob://")
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// fakeSiteStore is a SiteStore seeded with a starting bundleRef per site id.
// writes counts every UpdateBundleRef call, which is how
// TestSuccessfulPublishFlipsTheRowOnce proves the row is written exactly
// once per Publish, not once per uploaded file.
type fakeSiteStore struct {
	refs   map[string]string
	writes int
}

func newFakeSiteStore(seed map[string]string) *fakeSiteStore {
	refs := make(map[string]string, len(seed))
	for k, v := range seed {
		refs[k] = v
	}
	return &fakeSiteStore{refs: refs}
}

func (f *fakeSiteStore) UpdateBundleRef(_ context.Context, siteID, bundleRef string) error {
	f.writes++
	f.refs[siteID] = bundleRef
	return nil
}

func (f *fakeSiteStore) bundleRef(id string) string {
	return f.refs[id]
}

// bundleWith returns a Bundle of n files, always including index.html (a
// bundle without one is a different failure mode, covered separately below).
// Pure function of n: the same n always produces byte-identical content,
// which is what makes a republish of the same bundle a no-op version --
// exactly the property version()'s doc comment describes.
func bundleWith(n int) Bundle {
	b := Bundle{"index.html": []byte("<html>bundle</html>")}
	for i := 1; i < n; i++ {
		name := fmt.Sprintf("assets/file-%d.txt", i)
		b[name] = []byte(fmt.Sprintf("content of file %d", i))
	}
	return b
}

// ATOMICITY IS THE POINT. A partially-uploaded bundle must never be reachable:
// the whole thing lands under a NEW version prefix and only then does the row
// flip. A failure mid-upload leaves the previous version live and untouched.
func TestFailedUploadLeavesThePreviousVersionLive(t *testing.T) {
	store := newFakeBlobStore()
	sites := newFakeSiteStore(map[string]string{"s1": "blob://sites/s1/v1/"})
	p := NewPublisher(store, sites)

	store.failAfter = 2 // die partway through
	if _, err := p.Publish(t.Context(), "s1", bundleWith(4)); err == nil {
		t.Fatal("Publish succeeded despite an upload failure")
	}

	if got := sites.bundleRef("s1"); got != "blob://sites/s1/v1/" {
		t.Errorf("bundleRef moved to %q after a failed upload; it must still be v1", got)
	}
}

func TestSuccessfulPublishFlipsTheRowOnce(t *testing.T) {
	store := newFakeBlobStore()
	sites := newFakeSiteStore(map[string]string{"s1": "blob://sites/s1/v1/"})
	p := NewPublisher(store, sites)

	res, err := p.Publish(t.Context(), "s1", bundleWith(3))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if sites.writes != 1 {
		t.Errorf("the row was written %d times, want exactly 1", sites.writes)
	}
	if sites.bundleRef("s1") != res.BundleRef {
		t.Errorf("row says %q, publish returned %q", sites.bundleRef("s1"), res.BundleRef)
	}
	if res.BundleRef == "blob://sites/s1/v1/" {
		t.Error("publish reused the previous version prefix; versions must not be overwritten")
	}
}

// Rollback is the same write in the other direction, and the bytes must still
// be there. This is what versioned prefixes buy.
func TestRollbackPointsAtBytesThatStillExist(t *testing.T) {
	store := newFakeBlobStore()
	sites := newFakeSiteStore(map[string]string{"s1": "blob://sites/s1/v1/"})
	p := NewPublisher(store, sites)

	first, _ := p.Publish(t.Context(), "s1", bundleWith(2))
	if _, err := p.Publish(t.Context(), "s1", bundleWith(2)); err != nil {
		t.Fatal(err)
	}

	if !store.hasPrefix(first.BundleRef) {
		t.Error("the previous version's bytes were removed; rollback would 404")
	}
}

// version() must be a pure function of content: the same bundle, built
// twice, names the same version -- which is what makes a republish of
// identical bytes a no-op instead of a new version accumulating storage
// forever.
func TestVersionIsDeterministicOverContent(t *testing.T) {
	if version(bundleWith(3)) != version(bundleWith(3)) {
		t.Error("version() is not deterministic over identical content")
	}
}

// Different content must (in practice) name a different version -- the SHA
// changes, so a real deploy actually lands under a new prefix instead of
// silently colliding.
func TestVersionDiffersOverDifferentContent(t *testing.T) {
	if version(bundleWith(2)) == version(bundleWith(3)) {
		t.Error("version() collided for bundles with different content")
	}
}

func TestPublishRefusesAnEmptyBundle(t *testing.T) {
	p := NewPublisher(newFakeBlobStore(), newFakeSiteStore(nil))
	if _, err := p.Publish(t.Context(), "s1", Bundle{}); err == nil {
		t.Fatal("Publish accepted an empty bundle")
	}
}

// A bundle with no index.html serves nothing at "/" and nothing at any
// spa-fallback path -- refusing at publish time turns a broken build into a
// failed deploy rather than a live site that 404s its own homepage.
func TestPublishRefusesABundleWithNoIndexHTML(t *testing.T) {
	p := NewPublisher(newFakeBlobStore(), newFakeSiteStore(nil))
	b := Bundle{"assets/app.js": []byte("console.log('hi')")}
	if _, err := p.Publish(t.Context(), "s1", b); err == nil {
		t.Fatal("Publish accepted a bundle with no index.html")
	}
}

func TestPublishUploadsUnderTheReturnedVersionPrefix(t *testing.T) {
	store := newFakeBlobStore()
	sites := newFakeSiteStore(map[string]string{"s1": "blob://sites/s1/v1/"})
	p := NewPublisher(store, sites)

	res, err := p.Publish(t.Context(), "s1", bundleWith(2))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	wantKey := "sites/s1/" + res.Version + "/index.html"
	if _, ok := store.objects[wantKey]; !ok {
		t.Errorf("index.html was not uploaded under %q; got keys %v", wantKey, store.objects)
	}
}

// ---------------------------------------------------------------------------
// AzureUploader adapter -- the write-half mirror of blob_test.go's
// fakeAzureDownloader coverage for NewAzureBlobClient.
// ---------------------------------------------------------------------------

// fakeAzureUploader stands in for *azureblob.AzureBlobUploader's Upload
// method -- narrow enough that these tests exercise NewAzureBlobWriter's own
// adaptation logic (container plumbing, content-type selection) without an
// Azure SDK client or a live account.
type fakeAzureUploader struct {
	gotContainer   string
	gotObject      string
	gotData        []byte
	gotContentType string
	err            error
}

func (f *fakeAzureUploader) Upload(_ context.Context, container, objectName string, data []byte, contentType string) (string, error) {
	f.gotContainer = container
	f.gotObject = objectName
	f.gotData = data
	f.gotContentType = contentType
	if f.err != nil {
		return "", f.err
	}
	return "https://example.blob.core.windows.net/" + container + "/" + objectName, nil
}

func TestAzureBlobWriterPutPassesContainerKeyAndDataThrough(t *testing.T) {
	u := &fakeAzureUploader{}
	w := NewAzureBlobWriter(u, "sites-container")

	data := []byte("<html>v3")
	if err := w.Put(context.Background(), "sites/site-1/v3/index.html", data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if u.gotContainer != "sites-container" {
		t.Errorf("Upload called with container %q, want %q", u.gotContainer, "sites-container")
	}
	if u.gotObject != "sites/site-1/v3/index.html" {
		t.Errorf("Upload called with object %q, want the full key", u.gotObject)
	}
	if string(u.gotData) != string(data) {
		t.Errorf("Upload called with data %q, want %q", u.gotData, data)
	}
}

func TestAzureBlobWriterSurfacesUploadErrors(t *testing.T) {
	u := &fakeAzureUploader{err: errors.New("boom")}
	w := NewAzureBlobWriter(u, "sites-container")

	if err := w.Put(context.Background(), "index.html", []byte("x")); err == nil {
		t.Fatal("Put swallowed an Upload error")
	}
}

func TestBundleFileContentTypeUsesExtensionFirst(t *testing.T) {
	got := bundleFileContentType("index.html", []byte("<html></html>"))
	if !strings.Contains(got, "html") {
		t.Errorf("bundleFileContentType(%q) = %q, want it to name html", "index.html", got)
	}
}

// PNG's magic bytes are a stable sniffing target across Go versions, unlike
// asserting an exact mime.TypeByExtension string (its builtin table's exact
// charset formatting has changed between releases).
func TestBundleFileContentTypeFallsBackToSniffing(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	got := bundleFileContentType("no-extension", png)
	if got != "image/png" {
		t.Errorf("bundleFileContentType with no extension = %q, want image/png (sniffed)", got)
	}
}

func TestBundleFileContentTypeDefaultsToOctetStream(t *testing.T) {
	got := bundleFileContentType("no-extension", nil)
	if got != "application/octet-stream" {
		t.Errorf("bundleFileContentType for empty data = %q, want application/octet-stream", got)
	}
}

// ---------------------------------------------------------------------------
// engineSiteStore -- the production SiteStore. fakeEngine is edge_test.go's
// (same package), reused rather than redefined.
// ---------------------------------------------------------------------------

// The site concept declares @rowAuthz(clusterOwner); without a synthetic
// cluster-owner actor on the ctx handed to Execute, updateSiteBundle refuses
// the write the same way siteByHostname would refuse the read.
func TestEngineSiteStoreRunsUnderASyntheticClusterOwnerActor(t *testing.T) {
	fe := &fakeEngine{}
	s := NewEngineSiteStore(fe)

	if err := s.UpdateBundleRef(context.Background(), "s1", "blob://sites/s1/v1/"); err != nil {
		t.Fatalf("UpdateBundleRef: %v", err)
	}

	ac, ok := auth.AccessFromContext(fe.gotCtx)
	if !ok || ac == nil {
		t.Fatalf("engine.Execute ran with no AccessContext on ctx; updateSiteBundle will refuse this actor")
	}
	if !ac.IsClusterOwner() {
		t.Errorf("engine.Execute ran as role %q, want a cluster owner", ac.Role)
	}
}

// The write runs under its OWN synthetic identity, distinct from edge.go's
// systemEdgeActor -- see systemEdgePublishActor's comment for why the two
// must not be the same constant.
func TestEngineSiteStoreUsesItsOwnSyntheticIdentityNotEdgeGos(t *testing.T) {
	fe := &fakeEngine{}
	s := NewEngineSiteStore(fe)

	if err := s.UpdateBundleRef(context.Background(), "s1", "blob://sites/s1/v1/"); err != nil {
		t.Fatalf("UpdateBundleRef: %v", err)
	}

	ac, _ := auth.AccessFromContext(fe.gotCtx)
	if ac.UserId != systemEdgePublishActor {
		t.Errorf("actor UserId = %q, want %q", ac.UserId, systemEdgePublishActor)
	}
	if systemEdgePublishActor == systemEdgeActor {
		t.Fatal("systemEdgePublishActor must not equal edge.go's systemEdgeActor -- that constant's own comment says it is never used for anything else")
	}
}

// The invocation keyword must be "mutation" (call position), never "mutate"
// (the declaration verb) -- the parser rejects the latter in call position
// rather than silently dropping it (memql#2358), so getting this wrong would
// fail LOUD, but the point is to get it right, not just to fail loud.
func TestEngineSiteStoreCallsUpdateSiteBundleAsAMutation(t *testing.T) {
	fe := &fakeEngine{}
	s := NewEngineSiteStore(fe)

	if err := s.UpdateBundleRef(context.Background(), "s1", "blob://sites/s1/v2/"); err != nil {
		t.Fatalf("UpdateBundleRef: %v", err)
	}

	if !strings.HasPrefix(fe.gotQuery, "mutation updateSiteBundle(") {
		t.Errorf("query = %q, want it to start with %q", fe.gotQuery, "mutation updateSiteBundle(")
	}
	if !strings.Contains(fe.gotQuery, `siteId: "s1"`) {
		t.Errorf("query %q does not carry siteId", fe.gotQuery)
	}
	if !strings.Contains(fe.gotQuery, `bundleRef: "blob://sites/s1/v2/"`) {
		t.Errorf("query %q does not carry bundleRef", fe.gotQuery)
	}
}

// siteID / bundleRef reach the engine as quoted argument values, not
// interpolated unescaped into the statement.
func TestEngineSiteStoreQuotesArguments(t *testing.T) {
	fe := &fakeEngine{}
	s := NewEngineSiteStore(fe)

	if err := s.UpdateBundleRef(context.Background(), `s1".injected`, "blob://x/"); err != nil {
		t.Fatalf("UpdateBundleRef: %v", err)
	}
	if !strings.Contains(fe.gotQuery, `\"`) {
		t.Errorf("query %q does not look like an escaped siteId", fe.gotQuery)
	}
}

func TestEngineSiteStoreSurfacesEngineError(t *testing.T) {
	s := NewEngineSiteStore(&fakeEngine{err: errors.New("boom")})

	if err := s.UpdateBundleRef(context.Background(), "s1", "blob://sites/s1/v1/"); err == nil {
		t.Fatal("UpdateBundleRef swallowed an engine error")
	}
}
