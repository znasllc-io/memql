// component/edge/blob.go
package edge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

	"github.com/znasllc-io/memql/integrations/azureblob"
)

// ErrNotFound is what a BlobClient returns for a key that is not there. A
// distinct error rather than a nil slice, so "empty file" and "no file" stay
// distinguishable -- an empty index.html is a broken deploy, a missing one is
// a 404.
var ErrNotFound = errors.New("edge: object not found")

// BlobClient is the narrow read the edge needs from object storage. Narrow
// deliberately: the edge never writes (that is the publisher's job, Task 8)
// and never lists.
type BlobClient interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

type blobOpener struct{ client BlobClient }

// NewBlobOpener handles blob:// -- an uploaded bundle under a versioned
// prefix. Versions coexist, which is what makes rollback a single row write
// pointing back at bytes that are still there.
func NewBlobOpener(c BlobClient) BundleOpener { return blobOpener{client: c} }

func (b blobOpener) Open(ref string) (fs.FS, error) {
	const scheme = "blob://"
	if !strings.HasPrefix(ref, scheme) {
		return nil, fmt.Errorf("edge: bundleRef %q is not a blob:// reference", ref)
	}
	prefix := strings.TrimPrefix(ref, scheme)
	if prefix == "" {
		return nil, fmt.Errorf("edge: bundleRef %q names no prefix", ref)
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return &blobFS{client: b.client, prefix: prefix}, nil
}

// blobFS presents one bundle prefix as a filesystem.
//
// THE PREFIX IS A BOUNDARY, NOT A CONVENTION. The bundleRef comes from a row
// an operator wrote, but the request path comes from the internet, so the
// join is where a traversal would be introduced. fs.ValidPath rejects "..",
// leading slashes and empty segments outright, which is why the check is a
// refusal rather than a sanitising rewrite -- there is no legitimate request
// for a path outside the bundle to repair.
type blobFS struct {
	client BlobClient
	prefix string
}

func (b *blobFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	data, err := b.client.Get(context.Background(), b.prefix+name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &blobFile{name: name, data: data}, nil
}

type blobFile struct {
	name string
	data []byte
	off  int
}

// blobFile satisfies io.ReadSeeker. fs.File does not statically expose
// Seek, so http.ServeContent's own type assertion for range-request support
// depends on this holding even though nothing in the fs.File interface
// enforces it -- this compile-time check documents that contract where it
// can't silently rot.
var _ io.ReadSeeker = (*blobFile)(nil)

func (f *blobFile) Stat() (fs.FileInfo, error) {
	return blobInfo{name: f.name, size: int64(len(f.data))}, nil
}
func (f *blobFile) Close() error { return nil }

func (f *blobFile) Read(p []byte) (int, error) {
	if f.off >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += n
	return n, nil
}

// Seek is what makes http.ServeContent work -- range requests, and the
// Content-Length it sets without buffering. Without it every asset is served
// with chunked encoding and no range support.
func (f *blobFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(f.off) + offset
	case io.SeekEnd:
		abs = int64(len(f.data)) + offset
	default:
		return 0, fmt.Errorf("edge: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("edge: negative seek position")
	}
	f.off = int(abs)
	return abs, nil
}

type blobInfo struct {
	name string
	size int64
}

func (i blobInfo) Name() string       { return i.name }
func (i blobInfo) Size() int64        { return i.size }
func (i blobInfo) Mode() fs.FileMode  { return 0o444 }
func (i blobInfo) ModTime() time.Time { return time.Time{} }
func (i blobInfo) IsDir() bool        { return false }
func (i blobInfo) Sys() any           { return nil }

// AzureDownloader is the one method this adapter needs from an Azure blob
// client -- narrowed so a test can fake the read without an Azure SDK client
// or a live account. The assertion below pins *azureblob.AzureBlobUploader
// (integrations/azureblob, the storage integration's existing client, also
// behind integration.storage.upload) as satisfying it today, with no
// wrapping beyond what NewAzureBlobClient does.
type AzureDownloader interface {
	Download(ctx context.Context, container, objectName string) ([]byte, error)
}

var _ AzureDownloader = (*azureblob.AzureBlobUploader)(nil)

// azureBlobClient adapts an AzureDownloader to BlobClient, scoped to one
// container.
//
// THIS IS NOT A SECOND AZURE CLIENT. Connecting, auth and the write half
// (Upload, behind integration.storage.upload) stay owned entirely by
// integrations/azureblob; this type only ever calls Download, holding the
// edge's read to the same Get(ctx, key) shape every other BundleOpener
// backend gets. The edge never writes (that is the publisher's job, Task 8)
// and never lists, so nothing here reaches for more than Download.
type azureBlobClient struct {
	downloader AzureDownloader
	container  string
}

// NewAzureBlobClient adapts an already-constructed Azure blob downloader --
// in production, *azureblob.AzureBlobUploader -- to BlobClient. container
// should come from azureblob.ContainerFromEnv() (MEMQL_AZURE_BLOB_CONTAINER):
// the storage integration already owns that variable, so the edge reuses it
// rather than minting a second one.
func NewAzureBlobClient(d AzureDownloader, container string) BlobClient {
	return &azureBlobClient{downloader: d, container: container}
}

// Get downloads key from the configured container. Azure's not-found codes
// are translated to ErrNotFound so blobFS.Open sees the same miss shape
// (-> fs.ErrNotExist) regardless of which BlobClient is backing it; any
// other failure is returned wrapped, unmapped.
func (a *azureBlobClient) Get(ctx context.Context, key string) ([]byte, error) {
	data, err := a.downloader.Download(ctx, a.container, key)
	if err != nil {
		// BlobNotFound and ResourceNotFound both mean "this object is not
		// there" -- map both to ErrNotFound. If Azure ever answers with the
		// generic code instead of the blob-specific one, a legitimately
		// missing file must still 404, because the resolution order depends
		// on it: a missing <path>.html is supposed to fall through to the
		// next rung, not fail the whole request with a 503.
		//
		// ContainerNotFound is deliberately NOT mapped here. A missing
		// container is an infrastructure failure -- the entire bucket is
		// gone -- not a per-object miss. Answering 404 for that would hide a
		// real outage behind a response that looks like an ordinary missing
		// file: every site would silently 404 and nothing would page anyone.
		if bloberror.HasCode(err, bloberror.BlobNotFound, bloberror.ResourceNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("edge: download %q from container %q: %w", key, a.container, err)
	}
	return data, nil
}
