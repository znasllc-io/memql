package azureblob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/znasllc-io/memql/core/librarykey"
)

// bare_library_key_test.go -- a Library row may store a bare key rather than a
// URL (folder sync always does, and the chunked routes did), and the client's
// URL readers open it. These files are the row's own value through the client:
// a test that streams a full URL says nothing about them.

// The names a file really has. A URL parser would cut each of these somewhere,
// and a second decoding pass would name a different object.
var awkwardLibraryNames = []string{"notes.md", "space name%2F#?.tar", "literal%252F.txt", "report 100%.pdf", "日本語 レポート.pdf"}

func TestBareLibraryKeyOpensInTheLibraryContainerAsWritten(t *testing.T) {
	client, err := azblob.NewClientWithNoCredential("https://acct.blob.core.windows.net", nil)
	if err != nil {
		t.Fatal(err)
	}
	u := &AzureBlobUploader{client: client, libraryContainer: "memql-library"}
	for _, name := range awkwardLibraryNames {
		key := "library/u-1/f-1/" + name
		for _, stored := range []string{key, " " + key + "\n"} {
			container, object, ok := u.splitStoredURL(stored)
			if !ok || container != "memql-library" || object != key {
				t.Errorf("%q opened as %q %q %t, want the key as written in the Library's container", stored, container, object, ok)
			}
		}
	}
	for _, stored := range []string{
		"",
		// Bare, and not a key the Library writes: nothing on this path has any
		// business opening them, so they stay what they were.
		"sites/u-1/index.html",
		"backups/u-1/f-1/notes.md",
		"library",
		"/library/u-1/f-1/notes.md",
		"library\\u-1\\f-1\\notes.md",
		// A scheme this client cannot open is not a key with odd spelling.
		"local://library/u-1/f-1/notes.md",
		"gs://bucket/library/u-1/f-1/notes.md",
		"blob://library/u-1/f-1/notes.md",
		"library/u-1/f-1/odd://name",
	} {
		if container, object, ok := u.splitStoredURL(stored); ok {
			t.Errorf("%q opened as %q %q, want no object", stored, container, object)
		}
	}
}

// A client that knows no Library container opens no bare key, and never asks
// storage to guess: every other value it reads names its own container.
func TestBareLibraryKeyNeedsAConfiguredContainer(t *testing.T) {
	const key = "library/u-1/f-1/notes.md"
	client, err := azblob.NewClientWithNoCredential("https://acct.blob.core.windows.net", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, u := range map[string]*AzureBlobUploader{
		"nil client":   nil,
		"zero value":   {},
		"no container": {client: client},
		"no client":    {libraryContainer: "memql-library"},
	} {
		if _, _, ok := u.splitStoredURL(key); ok {
			t.Errorf("%s opened a bare key", name)
		}
	}
}

// New is the constructor every node reads through, and the container it reads is
// the one the upload routes write into.
func TestNewReadsTheLibraryContainerFromTheEnvironment(t *testing.T) {
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=a2V5;BlobEndpoint=http://127.0.0.1:1/devstoreaccount1;")
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", " memql-library ")
	u, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	container, object, ok := u.splitStoredURL("library/u-1/f-1/notes.md")
	if !ok || container != "memql-library" || object != "library/u-1/f-1/notes.md" {
		t.Fatalf("opened as %q %q %t", container, object, ok)
	}
}

// The read names the very object the blocks were staged and committed under, on
// the wire and whatever the name escapes to. Offline, so the ordinary test lanes
// hold this and not only the one with an emulator in it.
func TestBareLibraryKeyAsksStorageForTheObjectTheBlocksWentTo(t *testing.T) {
	for _, base := range []string{"https://acct.blob.core.windows.net", "http://azurite:10000/devstoreaccount1"} {
		for _, name := range awkwardLibraryNames {
			key := "library/u-1/f-1/" + name
			var paths []string
			transport := downloadTransport(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.Method+" "+r.URL.EscapedPath())
				status := http.StatusCreated
				if r.Method == http.MethodGet {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: http.Header{}, Body: io.NopCloser(strings.NewReader("x")), ContentLength: 1, Request: r}, nil
			})
			client, err := azblob.NewClientWithNoCredential(base, &azblob.ClientOptions{ClientOptions: policy.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}}})
			if err != nil {
				t.Fatal(err)
			}
			u := &AzureBlobUploader{client: client, libraryContainer: "memql-library"}
			ctx := context.Background()
			if err := u.StageBlock(ctx, "memql-library", key, blockIDForTest(1), []byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := u.CommitBlockList(ctx, "memql-library", key, []string{blockIDForTest(1)}, ""); err != nil {
				t.Fatal(err)
			}
			stream, err := u.DownloadStreamURL(ctx, key)
			if err != nil {
				t.Fatalf("%s %q: stream: %v", base, key, err)
			}
			_ = stream.Close()
			ranged, err := u.DownloadRangeURL(ctx, key, 0, 1)
			if err != nil {
				t.Fatalf("%s %q: range: %v", base, key, err)
			}
			_ = ranged.Close()
			if _, err := u.DownloadURL(ctx, key); err != nil {
				t.Fatalf("%s %q: buffered: %v", base, key, err)
			}
			if len(paths) != 5 {
				t.Fatalf("%s %q: storage saw %v, want five requests", base, key, paths)
			}
			written := strings.TrimPrefix(paths[0], http.MethodPut+" ")
			for _, path := range paths[:2] {
				if path != http.MethodPut+" "+written {
					t.Errorf("%s %q: the writes disagree: %v", base, key, paths)
				}
			}
			for _, path := range paths[2:] {
				if path != http.MethodGet+" "+written {
					t.Errorf("%s %q: a read asked for %q, but the blocks went to %q", base, key, path, written)
				}
			}
		}
	}
}

// StoredURL is what the chunked routes put on a row: the value Upload answers for
// the same object, with nothing of the connection in it, and it reads back as that
// object.
func TestStoredURLNamesTheObjectAndCarriesNoCredential(t *testing.T) {
	for _, base := range []string{"https://acct.blob.core.windows.net", "http://azurite:10000/devstoreaccount1", "https://acct.blob.core.windows.net/?sig=private"} {
		client, err := azblob.NewClientWithNoCredential(base, nil)
		if err != nil {
			t.Fatal(err)
		}
		u := &AzureBlobUploader{client: client}
		for _, name := range awkwardLibraryNames {
			key := "library/u-1/f-1/" + name
			stored, err := u.StoredURL("memql-library", key)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stored, "sig=") || !strings.Contains(stored, "://") {
				t.Errorf("%s: stored %q", base, stored)
			}
			if container, object, ok := u.splitStoredURL(stored); !ok || container != "memql-library" || object != key {
				t.Errorf("%s: %q reads back as %q %q %t", base, stored, container, object, ok)
			}
			if err := librarykey.Check(stored, "u-1", "memql-library"); err != nil {
				t.Errorf("%s: the owner's own %q was refused: %v", base, stored, err)
			}
		}
	}
	if _, err := (&AzureBlobUploader{}).StoredURL("memql-library", "library/u-1/f-1/notes.md"); err == nil {
		t.Fatal("a client with no connection named a stored URL")
	}
}

// Against real block storage: blocks staged and committed under a key, then read
// through every URL reader by the value a row stores.
func TestAzuriteBareLibraryKeyOpensTheCommittedBlob(t *testing.T) {
	u := azuriteUploader(t)
	u.libraryContainer = "blockstest"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first, second := []byte("first chunk, "), []byte("second chunk")
	whole := append(append([]byte{}, first...), second...)
	for _, name := range awkwardLibraryNames {
		key := fmt.Sprintf("library/u-1/f-%d/%s", time.Now().UnixNano(), name)
		if err := u.StageBlock(ctx, "blockstest", key, blockIDForTest(1), first); err != nil {
			t.Fatal(err)
		}
		if err := u.StageBlock(ctx, "blockstest", key, blockIDForTest(2), second); err != nil {
			t.Fatal(err)
		}
		if err := u.CommitBlockList(ctx, "blockstest", key, []string{blockIDForTest(1), blockIDForTest(2)}, "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		// The check vouches for the object the read then opens.
		if err := librarykey.Check(key, "u-1", "blockstest"); err != nil {
			t.Fatalf("the owner's own key %q was refused: %v", key, err)
		}
		stream, err := u.DownloadStreamURL(ctx, key)
		if err != nil {
			t.Fatalf("stream %q: %v", key, err)
		}
		streamed, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil || !bytes.Equal(streamed, whole) {
			t.Fatalf("stream %q = %q, %v", key, streamed, err)
		}
		ranged, err := u.DownloadRangeURL(ctx, key, int64(len(first))-1, 3)
		if err != nil {
			t.Fatalf("range %q: %v", key, err)
		}
		part, err := io.ReadAll(ranged)
		_ = ranged.Close()
		if want := whole[len(first)-1 : len(first)+2]; err != nil || !bytes.Equal(part, want) {
			t.Fatalf("range %q = %q, %v, want %q", key, part, err, want)
		}
		buffered, err := u.DownloadURL(ctx, key)
		if err != nil || !bytes.Equal(buffered, whole) {
			t.Fatalf("buffered %q = %q, %v", key, buffered, err)
		}
	}

	// A key nothing landed at (a one-shot upload storage refused keeps its
	// intended key on the row) is storage's answer now, and storage says so.
	if _, err := u.DownloadStreamURL(ctx, "library/u-1/never-landed/notes.md"); !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("a key with no object answered %v", err)
	}

	// Only the Library's container: the same key in another one stays out of reach.
	const other = "blockstest-other"
	if err := u.EnsureContainer(ctx, other); err != nil {
		t.Fatal(err)
	}
	const elsewhere = "library/u-1/elsewhere/notes.md"
	if _, err := u.Upload(ctx, other, elsewhere, []byte("another container's bytes"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := u.DownloadStreamURL(ctx, elsewhere); !bloberror.HasCode(err, bloberror.BlobNotFound) {
		t.Fatalf("a bare key reached another container: %v", err)
	}
}
