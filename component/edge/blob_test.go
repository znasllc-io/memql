// component/edge/blob_test.go
package edge

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

type stubBlob struct {
	objects map[string][]byte
}

func (s *stubBlob) Get(_ context.Context, key string) ([]byte, error) {
	b, ok := s.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

func TestBlobSchemeReadsAVersionedPrefix(t *testing.T) {
	c := &stubBlob{objects: map[string][]byte{
		"sites/site-1/v3/index.html": []byte("<html>v3"),
	}}

	fsys, err := NewBlobOpener(c).Open("blob://sites/site-1/v3/")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := fsReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("reading index.html: %v", err)
	}
	if string(b) != "<html>v3" {
		t.Errorf("got %q", b)
	}
}

// Two versions coexist. That is the entire reason bundles go under a
// versioned prefix rather than overwriting: rollback is a row write, and the
// bytes it points back at have to still be there.
func TestBlobVersionsCoexist(t *testing.T) {
	c := &stubBlob{objects: map[string][]byte{
		"sites/site-1/v2/index.html": []byte("<html>v2"),
		"sites/site-1/v3/index.html": []byte("<html>v3"),
	}}
	o := NewBlobOpener(c)

	for ref, want := range map[string]string{
		"blob://sites/site-1/v2/": "<html>v2",
		"blob://sites/site-1/v3/": "<html>v3",
	} {
		fsys, err := o.Open(ref)
		if err != nil {
			t.Fatalf("Open(%q): %v", ref, err)
		}
		b, _ := fsReadFile(fsys, "index.html")
		if string(b) != want {
			t.Errorf("Open(%q) served %q, want %q", ref, b, want)
		}
	}
}

// A bundleRef must not be able to read outside its own prefix. The ref comes
// from a row an operator wrote, but the request path comes from the internet.
func TestBlobRefusesPathEscape(t *testing.T) {
	c := &stubBlob{objects: map[string][]byte{"secrets/key": []byte("nope")}}

	fsys, _ := NewBlobOpener(c).Open("blob://sites/site-1/v3/")
	if _, err := fsReadFile(fsys, "../../../secrets/key"); err == nil {
		t.Error("the blob opener served a path outside its prefix")
	}
}

func TestMuxOpenerRoutesByScheme(t *testing.T) {
	mux := NewMuxOpener(map[string]BundleOpener{
		"file": NewFileOpener(),
		"blob": NewBlobOpener(&stubBlob{objects: map[string][]byte{}}),
	})
	if _, err := mux.Open("gopher://x"); err == nil {
		t.Error("the mux accepted an unknown scheme")
	}
}

// fakeAzureDownloader stands in for *azureblob.AzureBlobUploader's Download
// method -- narrow enough that these tests exercise NewAzureBlobClient's own
// adaptation logic (container plumbing, the BlobNotFound -> ErrNotFound
// translation) without an Azure SDK client or a live account.
type fakeAzureDownloader struct {
	gotContainer string
	gotObject    string
	data         []byte
	err          error
}

func (f *fakeAzureDownloader) Download(_ context.Context, container, objectName string) ([]byte, error) {
	f.gotContainer = container
	f.gotObject = objectName
	if f.err != nil {
		return nil, f.err
	}
	return f.data, nil
}

func TestAzureBlobClientGetPassesContainerAndKeyThrough(t *testing.T) {
	d := &fakeAzureDownloader{data: []byte("<html>v3")}
	c := NewAzureBlobClient(d, "sites-container")

	b, err := c.Get(context.Background(), "sites/site-1/v3/index.html")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(b) != "<html>v3" {
		t.Errorf("got %q", b)
	}
	if d.gotContainer != "sites-container" {
		t.Errorf("Download called with container %q, want %q", d.gotContainer, "sites-container")
	}
	if d.gotObject != "sites/site-1/v3/index.html" {
		t.Errorf("Download called with object %q, want the full key", d.gotObject)
	}
}

// A bloberror.BlobNotFound response must become ErrNotFound -- that is the
// distinction blobFS.Open depends on to answer fs.ErrNotExist instead of a
// generic open failure, for blob:// exactly as for any other BlobClient.
func TestAzureBlobClientMapsBlobNotFoundToErrNotFound(t *testing.T) {
	d := &fakeAzureDownloader{err: &azcore.ResponseError{ErrorCode: string(bloberror.BlobNotFound)}}
	c := NewAzureBlobClient(d, "sites-container")

	_, err := c.Get(context.Background(), "missing.html")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get returned %v, want ErrNotFound", err)
	}
}

// Any other failure (network, auth, a different service error) must surface
// as its own error rather than be folded into ErrNotFound -- those are a 502
// and a 404 respectively to whatever ends up calling this.
func TestAzureBlobClientSurfacesOtherErrors(t *testing.T) {
	d := &fakeAzureDownloader{err: &azcore.ResponseError{ErrorCode: string(bloberror.AuthorizationFailure)}}
	c := NewAzureBlobClient(d, "sites-container")

	_, err := c.Get(context.Background(), "index.html")
	if err == nil {
		t.Fatal("Get swallowed a non-not-found error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("Get mapped a non-not-found error to ErrNotFound")
	}
}
