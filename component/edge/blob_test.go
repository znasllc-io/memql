// component/edge/blob_test.go
package edge

import (
	"context"
	"errors"
	"io"
	"io/fs"
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

// An empty stored object is a successful open with zero bytes, not a miss.
// ErrNotFound means "not there"; a present-but-empty object is a different
// (and suspicious) situation the caller must be able to tell apart from it --
// per ErrNotFound's own doc comment, an empty index.html is a broken deploy,
// a missing one is a 404. Both cases are exercised here, against the SAME
// fsys, so the test actually proves the distinction rather than just the
// empty half of it.
func TestBlobSchemeDistinguishesEmptyObjectFromMissing(t *testing.T) {
	c := &stubBlob{objects: map[string][]byte{
		"sites/site-1/v3/index.html": {},
	}}
	fsys, err := NewBlobOpener(c).Open("blob://sites/site-1/v3/")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	b, err := fsReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("reading a present-but-empty object returned an error: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("got %q, want zero bytes", b)
	}

	if _, err := fsReadFile(fsys, "missing.html"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reading a missing object returned %v, want fs.ErrNotExist", err)
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

// The Seek tests below construct *blobFile directly rather than going
// through NewBlobOpener/stubBlob: Seek is what makes http.ServeContent work
// (range requests, and the Content-Length it sets without buffering), and
// nothing else in this package calls it, so a regression here would
// otherwise be invisible.

func TestBlobFileSeekStart(t *testing.T) {
	f := &blobFile{data: []byte("0123456789")}
	pos, err := f.Seek(3, io.SeekStart)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if pos != 3 {
		t.Errorf("Seek returned %d, want 3", pos)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll after Seek: %v", err)
	}
	if string(b) != "3456789" {
		t.Errorf("read after Seek(3, SeekStart) = %q, want %q", b, "3456789")
	}
}

func TestBlobFileSeekCurrent(t *testing.T) {
	f := &blobFile{data: []byte("0123456789")}
	if _, err := f.Seek(2, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	pos, err := f.Seek(3, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if pos != 5 {
		t.Errorf("Seek returned %d, want 5", pos)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll after Seek: %v", err)
	}
	if string(b) != "56789" {
		t.Errorf("read after Seek(3, SeekCurrent) from offset 2 = %q, want %q", b, "56789")
	}
}

func TestBlobFileSeekEnd(t *testing.T) {
	f := &blobFile{data: []byte("0123456789")}
	pos, err := f.Seek(-4, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if pos != 6 {
		t.Errorf("Seek returned %d, want 6", pos)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll after Seek: %v", err)
	}
	if string(b) != "6789" {
		t.Errorf("read after Seek(-4, SeekEnd) = %q, want %q", b, "6789")
	}
}

func TestBlobFileSeekNegativeResultIsRefused(t *testing.T) {
	f := &blobFile{data: []byte("0123456789")}
	if _, err := f.Seek(-1, io.SeekStart); err == nil {
		t.Error("Seek to a negative position succeeded, want an error")
	}
}

func TestBlobFileSeekInvalidWhenceIsRefused(t *testing.T) {
	f := &blobFile{data: []byte("0123456789")}
	if _, err := f.Seek(0, 99); err == nil {
		t.Error("Seek with an invalid whence succeeded, want an error")
	}
}

// Seeking past the end and then reading must behave exactly like reading an
// exhausted file -- io.EOF, not a panic and not garbage bytes.
// http.ServeContent depends on this for a Range request that grazes the end
// of the file.
func TestBlobFileSeekPastEndThenReadIsEOF(t *testing.T) {
	f := &blobFile{data: []byte("0123456789")}
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	b := make([]byte, 4)
	n, err := f.Read(b)
	if n != 0 || err != io.EOF {
		t.Errorf("Read past end = (%d, %v), want (0, io.EOF)", n, err)
	}
}

// A read, a seek back to the start, and a second read must return the same
// bytes -- the property http.ServeContent depends on when it seeks back to
// satisfy a Range header after already having probed the file.
func TestBlobFileReadSeekRead(t *testing.T) {
	f := &blobFile{data: []byte("0123456789")}

	first := make([]byte, 4)
	if _, err := io.ReadFull(f, first); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if string(first) != "0123" {
		t.Fatalf("first read = %q, want %q", first, "0123")
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	second := make([]byte, 4)
	if _, err := io.ReadFull(f, second); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if string(second) != "0123" {
		t.Errorf("second read after seeking back to start = %q, want %q", second, "0123")
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

// bloberror.ResourceNotFound must map to ErrNotFound exactly like
// BlobNotFound does. It is Azure's generic not-found code; if it ever went
// unmapped, a legitimately missing file would surface as a 503 instead of a
// 404 in whatever configuration returns it -- breaking the resolution order,
// since a missing <path>.html is supposed to fall through to the next rung,
// not fail the request.
func TestAzureBlobClientMapsResourceNotFoundToErrNotFound(t *testing.T) {
	d := &fakeAzureDownloader{err: &azcore.ResponseError{ErrorCode: string(bloberror.ResourceNotFound)}}
	c := NewAzureBlobClient(d, "sites-container")

	_, err := c.Get(context.Background(), "missing.html")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get returned %v, want ErrNotFound", err)
	}
}

// bloberror.ContainerNotFound must NOT map to ErrNotFound. A missing
// container is an infrastructure failure -- the entire bucket is gone -- not
// a per-object miss, and folding it into ErrNotFound would hide a real
// outage behind a response that looks like an ordinary missing file: every
// site would silently 404 and nothing would page anyone.
func TestAzureBlobClientDoesNotMapContainerNotFoundToErrNotFound(t *testing.T) {
	d := &fakeAzureDownloader{err: &azcore.ResponseError{ErrorCode: string(bloberror.ContainerNotFound)}}
	c := NewAzureBlobClient(d, "sites-container")

	_, err := c.Get(context.Background(), "index.html")
	if err == nil {
		t.Fatal("Get swallowed a ContainerNotFound error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("Get mapped ContainerNotFound to ErrNotFound -- a missing container is an outage, not a per-object miss")
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
