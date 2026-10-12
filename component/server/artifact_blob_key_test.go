package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
)

// artifact_blob_key_test.go -- GET /artifacts/{id}/content holds a file row's
// blobUrl to its owner's own library/{owner}/ prefix before it reads a byte.
//
// createLibraryFile being @serverOnly is what stops a client WRITING such a
// row. This is what stops the route SERVING one: a row written before that
// was true, or by a server writer that forwarded a key it did not compose.
// The reader owns the row in every case below, so row admission passes; the
// key is the only thing left to refuse on.

// askedDownloader records every object the route asked storage for, so a
// refusal can be shown to happen BEFORE the read rather than after it.
type askedDownloader struct {
	inner FileDownloader
	asked []string
}

func (d *askedDownloader) DownloadURL(ctx context.Context, blobURL string) ([]byte, error) {
	d.asked = append(d.asked, blobURL)
	return d.inner.DownloadURL(ctx, blobURL)
}

type askedStreamer struct {
	inner *fakeStreamer
	asked []string
}

func (s *askedStreamer) DownloadStreamURL(ctx context.Context, blobURL string) (io.ReadCloser, error) {
	s.asked = append(s.asked, blobURL)
	return s.inner.DownloadStreamURL(ctx, blobURL)
}

func (s *askedStreamer) DownloadRangeURL(ctx context.Context, blobURL string, offset, count int64) (io.ReadCloser, error) {
	s.asked = append(s.asked, blobURL)
	return s.inner.DownloadRangeURL(ctx, blobURL, offset, count)
}

// seedForgedFile seeds user-a's file, then user-b's own file row pointing at
// user-a's key -- the row a client createLibraryFile could write before it was
// @serverOnly. Returns user-a's bytes.
func seedForgedFile(store *fakeLibraryStore, blob *fakeBlob) []byte {
	secret := []byte("employee,salary\nalex,182000\n")
	seedFileArtifact(store, blob, "user-a", "artifact-a", "file-a", secret, "text/csv", "payroll.csv")
	seedFileArtifact(store, blob, "user-b", "artifact-b", "file-b", []byte("b"), "text/csv", "mine.csv")
	store.files[LibraryFileConceptRef("file-b")].BlobUrl = store.files[LibraryFileConceptRef("file-a")].BlobUrl
	store.files[LibraryFileConceptRef("file-b")].Size = len(secret)
	return secret
}

func TestContentRefusesAFileRowNamingAnotherOwnersKey(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	secret := seedForgedFile(store, blob)
	dl := &askedDownloader{inner: blob}
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Bucket: "lib", Downloader: dl, Store: store})

	rec := getContentVersion(t, h, "user-b", "artifact-b", "")
	if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), secret) {
		t.Fatalf("B's row naming A's key answered %d with %q -- the route served bytes that are not "+
			"under B's own library/ prefix", rec.Code, rec.Body.String())
	}
	if len(dl.asked) != 0 {
		t.Fatalf("storage was asked for %q before the refusal -- the key must be judged before it is read", dl.asked)
	}

	// The owner still reads their own file through the same route.
	if rec := getContentVersion(t, h, "user-a", "artifact-a", ""); rec.Code != http.StatusOK ||
		!bytes.Equal(rec.Body.Bytes(), secret) {
		t.Fatalf("A's own download answered %d: %q", rec.Code, rec.Body.String())
	}
}

// The constant-memory path reads through a different seam, and a Range
// request through a third method on it. Both are judged first.
func TestContentRefusesAForeignKeyOnTheStreamingPath(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	secret := seedForgedFile(store, blob)
	streamer := &askedStreamer{inner: &fakeStreamer{objects: blob.objects}}
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Bucket: "lib", Streamer: streamer, Store: store})

	for _, rangeHeader := range []string{"", "bytes=0-5"} {
		rec := getContentWithRange(t, h, "user-b", "artifact-b", rangeHeader)
		if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), secret[:6]) {
			t.Fatalf("range %q: B's row naming A's key answered %d with %q", rangeHeader, rec.Code, rec.Body.String())
		}
	}
	if len(streamer.asked) != 0 {
		t.Fatalf("storage was asked for %q before the refusal", streamer.asked)
	}
}

// A version row carries a key of its own, copied from the head it froze. A
// forged head that was superseded leaves a forged VERSION behind, so
// ?version={n} is judged on the version's key, against the file's owner.
func TestContentRefusesAForeignKeyOnAnOlderVersion(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	secret := seedForgedFile(store, blob)
	foreign := store.files[LibraryFileConceptRef("file-a")].BlobUrl
	store.files[LibraryFileConceptRef("file-b")].BlobUrl = "https://example.blob.core.windows.net/lib/library/user-b/file-b/k-2/mine.csv"
	store.files[LibraryFileConceptRef("file-b")].VersionNumber = 2
	store.versions["file-b"] = map[int]*LibraryFileVersionRow{
		1: {ID: "file-b-v1", FileId: "file-b", VersionNumber: 1, Name: "mine.csv", MimeType: "text/csv",
			Size: len(secret), BlobUrl: foreign},
	}
	dl := &askedDownloader{inner: blob}
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Bucket: "lib", Downloader: dl, Store: store})

	rec := getContentVersion(t, h, "user-b", "artifact-b", "1")
	if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), secret) {
		t.Fatalf("B's version 1 naming A's key answered %d with %q", rec.Code, rec.Body.String())
	}
	if len(dl.asked) != 0 {
		t.Fatalf("storage was asked for %q before the refusal", dl.asked)
	}
}

// The node reads every container its credential reaches, so a key under the
// caller's own prefix in ANOTHER container is not the caller's Library either.
func TestContentRefusesAKeyInAnotherContainer(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedFileArtifact(store, blob, "user-b", "artifact-b", "file-b", []byte("b"), "text/plain", "b.txt")
	other := "https://example.blob.core.windows.net/logs-archive/library/user-b/file-b/b.txt"
	blob.objects[other] = []byte("an operator's log archive")
	store.files[LibraryFileConceptRef("file-b")].BlobUrl = other
	dl := &askedDownloader{inner: blob}
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Bucket: "lib", Downloader: dl, Store: store})

	if rec := getContentVersion(t, h, "user-b", "artifact-b", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a key in another container answered %d: %q", rec.Code, rec.Body.String())
	}
	if len(dl.asked) != 0 {
		t.Fatalf("storage was asked for %q before the refusal", dl.asked)
	}
}

// The stored owner is canonical while the upload route writes the bare access
// id into the path. One person, two spellings: the owner's download must not
// turn into a 404 over it.
func TestContentServesTheOwnersKeyWhenTheRowOwnerIsCanonical(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	data := []byte("mine")
	seedFileArtifact(store, blob, "user-a", "artifact-a", "file-a", data, "text/plain", "a.txt")
	store.files[LibraryFileConceptRef("file-a")].OwnerUserId = "v1:identity:user:user-a"
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Bucket: "lib", Downloader: blob, Store: store})

	if rec := getContentVersion(t, h, "user-a", "artifact-a", ""); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("the owner's own file answered %d: %q", rec.Code, rec.Body.String())
	}
}
