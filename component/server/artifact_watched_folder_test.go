package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// artifact_watched_folder_test.go -- the (machine, path) re-push, epic
// memql#4783's engine half.
//
// The epic's headline is that a folder on somebody's machine stays backed up
// and visibly in sync, and the whole of that rests on one property: a file
// pushed twice from the same place is ONE logical file with two versions, not
// two rows. Everything else in the epic -- the link states, the folder rollup,
// the Files app's badges -- reads a row this resolve either found or did not.
//
// Four claims, each of which fails without its own line of the implementation:
//
//  1. A re-push from the SAME (machine, path) versions the SAME file. No
//     duplicate row, no second artifact, and the response names the artifact
//     that already existed.
//  2. A DIFFERENT path on the same machine is a different file. The key is
//     both halves; a resolve that matched on the machine alone would fold a
//     person's whole watched folder into one row.
//  3. A browser upload keys on NOTHING and never resolves. It has no honest
//     machine or path identity to key on, and matching by filename would
//     silently merge two different files -- the reasoning memql#4721's D5
//     settled and this does not reopen.
//  4. An ARCHIVED row is not the live copy. A re-push after somebody emptied
//     the file into the Bin starts a fresh file rather than writing new bytes
//     into a row nobody can see.

const watchedPath = "/Users/a/Clients/acme/cut-03.mov"

// seedPushedFile seeds a file that arrived from a machine at a path, with its
// promotion landed -- the state a first cockpit push leaves behind.
func seedPushedFile(store *fakeLibraryStore, blob *fakeBlob, owner, artifactId, fileId,
	workerId, path string, data []byte) {
	seedFileArtifact(store, blob, owner, artifactId, fileId, data, "video/quicktime", "cut-03.mov")
	row := store.files[LibraryFileConceptRef(fileId)]
	row.UploadedFromWorkerId = workerId
	row.UploadedFromPath = path
	row.UploadedFromWorkerName = "MacBook-Pro"
	row.VersionNumber = 1
	row.VersionUploadedAt = "2026-08-01T10:00:00Z"
	store.artifactForFile[fileId] = artifactId
}

func TestARePushFromTheSameMachineAndPathVersionsTheSameFile(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	seedPushedFile(store, blob, "user-a", "art-cut", "file-cut", "wrk-1", watchedPath, []byte("take one"))

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "cut-03.mov", "video/quicktime",
		[]byte("take two, longer"), map[string]string{
			"uploadedFromWorkerId": "wrk-1",
			"uploadedFromPath":     watchedPath,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	// No second file row: the re-push superseded, it did not create.
	if created := store.snapshotCreated(); len(created) != 0 {
		t.Fatalf("createLibraryFile ran %d time(s) on a re-push; the watched folder would grow a "+
			"new row per save and the Files list would fill with copies of one file: %+v",
			len(created), created)
	}
	supersedes := store.snapshotSupersedes()
	if len(supersedes) != 1 {
		t.Fatalf("SupersedeFile ran %d times, want 1", len(supersedes))
	}
	if got := supersedes[0].head.FileId; got != "file-cut" {
		t.Errorf("the new head landed on file %q, want file-cut -- the re-push must keep the "+
			"file's identity", got)
	}
	if got := supersedes[0].head.VersionNumber; got != 2 {
		t.Errorf("new version = %d, want 2", got)
	}
	// The frozen version keeps ITS OWN provenance, which is what lets the
	// history say which push produced which version.
	if got := supersedes[0].snap.UploadedFromPath; got != watchedPath {
		t.Errorf("frozen version's path = %q, want %q", got, watchedPath)
	}
	if !strings.Contains(rec.Body.String(), "art-cut") {
		t.Errorf("the response named a different artifact than the one that already existed: %s",
			rec.Body.String())
	}
}

func TestARePushStampsTheLinkBackToSynced(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	seedPushedFile(store, blob, "user-a", "art-cut", "file-cut", "wrk-1", watchedPath, []byte("take one"))
	// The verify lane had noticed the origin move on.
	store.files[LibraryFileConceptRef("file-cut")].LinkState = "stale"

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "cut-03.mov", "video/quicktime",
		[]byte("take two"), map[string]string{
			"uploadedFromWorkerId": "wrk-1",
			"uploadedFromPath":     watchedPath,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if got := store.linkStateOf("file-cut"); got != "synced" {
		t.Errorf("link state after a re-push = %q, want synced -- receiving the bytes IS the "+
			"evidence that the copy equals the origin, and a re-push is what clears a stale",
			got)
	}
}

func TestABrowserUploadStampsNoLinkStateAtAll(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "notes.txt", "text/plain",
		[]byte("typed here"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if got := store.linkStateOf(store.snapshotCreated()[0].FileId); got != "" {
		t.Errorf("link state = %q on a file with no origin to link to; absent is not a fourth "+
			"state and must not be spelled like one", got)
	}
}

func TestADifferentPathOnTheSameMachineIsADifferentFile(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	seedPushedFile(store, blob, "user-a", "art-cut", "file-cut", "wrk-1", watchedPath, []byte("take one"))

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "cut-04.mov", "video/quicktime",
		[]byte("a different clip"), map[string]string{
			"uploadedFromWorkerId": "wrk-1",
			"uploadedFromPath":     "/Users/a/Clients/acme/cut-04.mov",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if len(store.snapshotSupersedes()) != 0 {
		t.Error("a different path superseded an existing file; the key is BOTH halves, and " +
			"matching on the machine alone folds a whole watched folder into one row")
	}
	if len(store.snapshotCreated()) != 1 {
		t.Errorf("createLibraryFile ran %d times, want 1", len(store.snapshotCreated()))
	}
}

// A BROWSER UPLOAD NEVER RESOLVES, even when a file with the same name is
// already in the Library from the same person. The reachable positive is the
// test above: the resolve DOES fire when a key is present, so an untouched
// row here is the key's absence rather than a resolve that stopped working.
func TestABrowserUploadNeverResolvesAKeyedTarget(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	seedPushedFile(store, blob, "user-a", "art-cut", "file-cut", "wrk-1", watchedPath, []byte("take one"))

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "cut-03.mov", "video/quicktime",
		[]byte("the same name, from a laptop's downloads folder"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if len(store.snapshotSupersedes()) != 0 {
		t.Error("a browser upload superseded a file it happens to share a name with -- which is " +
			"exactly the silent merge the named-target design exists to prevent")
	}
}

// AN ARCHIVED ROW IS NOT THE LIVE COPY. The fake's resolve mirrors the query's
// `archived != true` conjunct, so a re-push after somebody emptied the file
// into the Bin writes a fresh file rather than new bytes into a row nobody can
// see.
func TestARePushAfterArchivingStartsAFreshFile(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	seedPushedFile(store, blob, "user-a", "art-cut", "file-cut", "wrk-1", watchedPath, []byte("take one"))
	store.files[LibraryFileConceptRef("file-cut")].Archived = true

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "cut-03.mov", "video/quicktime",
		[]byte("take two"), map[string]string{
			"uploadedFromWorkerId": "wrk-1",
			"uploadedFromPath":     watchedPath,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if len(store.snapshotSupersedes()) != 0 {
		t.Error("the re-push versioned an ARCHIVED row: the person would see nothing arrive and " +
			"the backup would report success")
	}
	if len(store.snapshotCreated()) != 1 {
		t.Errorf("createLibraryFile ran %d times, want 1", len(store.snapshotCreated()))
	}
}

// A NAMED TARGET OUTRANKS THE KEY. Somebody pointing at a row in the inspector
// has made a decision, and a key that quietly disagreed would write their
// bytes somewhere else.
func TestANamedTargetOutranksTheProvenanceKey(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	seedPushedFile(store, blob, "user-a", "art-cut", "file-cut", "wrk-1", watchedPath, []byte("take one"))
	seedVersionedFile(store, blob, "user-a", "art-other", "file-other", []byte("elsewhere"),
		"text/plain", "notes.txt", 1)

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "cut-03.mov", "video/quicktime",
		[]byte("take two"), map[string]string{
			"targetArtifactId":     "art-other",
			"uploadedFromWorkerId": "wrk-1",
			"uploadedFromPath":     watchedPath,
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	supersedes := store.snapshotSupersedes()
	if len(supersedes) != 1 {
		t.Fatalf("SupersedeFile ran %d times, want 1", len(supersedes))
	}
	if got := supersedes[0].head.FileId; got != "file-other" {
		t.Errorf("the bytes landed on %q; a named target must win over the key", got)
	}
}

// A RESOLVE THAT FAILED IS NOT AN ABSENCE. If the key read errors, the upload
// refuses rather than quietly becoming a new file -- a silent fallback would
// grow a duplicate on every failed read and report success each time.
func TestAFailedKeyResolveRefusesRatherThanDuplicating(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	store.fileByUploadedFromErr = errors.New("the graph is unreachable")

	rec := postUpload(t, newWatchedHandler(store, blob), "user-a", "cut-03.mov", "video/quicktime",
		[]byte("take two"), map[string]string{
			"uploadedFromWorkerId": "wrk-1",
			"uploadedFromPath":     watchedPath,
		})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 -- a resolve that could not run is not the same answer "+
			"as a resolve that found nothing; body: %s", rec.Code, rec.Body.String())
	}
	if len(store.snapshotCreated()) != 0 {
		t.Error("a file row was written after the resolve failed; the refusal must land before " +
			"anything is created")
	}
	if blob.objectCount() != 0 {
		t.Error("bytes reached storage after the resolve failed; every refusal on this route " +
			"lands before a byte is stored")
	}
}

func newWatchedHandler(store *fakeLibraryStore, blob *fakeBlob) *ArtifactHandler {
	return NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b", Uploader: blob, Downloader: blob,
		Store: store, PromotionWait: time10ms(), PromotionPoll: time1ms(),
	})
}

// THE CHUNKED ROUTE NEEDS THE KEYED RESOLVE MORE THAN THE ONE-SHOT ONE DOES.
//
// The epic's own scenario is a person producing client video: every file in
// that watched folder is past the one-shot threshold, so every re-push a
// watcher makes arrives here. A keyed resolve on the one-shot path alone
// versions small files and duplicates large ones -- which works in a demo and
// is broken for the feature it was built for.
func TestAChunkedRePushFromTheSameMachineAndPathVersionsTheSameFile(t *testing.T) {
	store := newFakeLibraryStore()
	sessions := newFakeSessionStore()
	blocks := newFakeBlocks()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	seedPushedFile(store, newFakeBlob(), "user-a", "art-cut", "file-cut", "wrk-1", watchedPath,
		[]byte("take one"))
	h := sessionsHandler(store, sessions, blocks, 4, nil)

	out := openSession(t, h, "user-a", map[string]any{
		"name":                 "cut-03.mov",
		"size":                 8,
		"uploadedFromWorkerId": "wrk-1",
		"uploadedFromPath":     watchedPath,
	})
	for n, part := range [][]byte{[]byte("take"), []byte("-two")} {
		if rec := putChunk(t, h, "user-a", out.UploadId, n+1, part); rec.Code != http.StatusNoContent {
			t.Fatalf("chunk %d = %d; body: %s", n+1, rec.Code, rec.Body.String())
		}
	}
	rec := doJSON(t, h, http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "user-a", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	if created := store.snapshotCreated(); len(created) != 0 {
		t.Fatalf("createLibraryFile ran %d time(s) on a chunked re-push; the watched folder would "+
			"grow a new row per save: %+v", len(created), created)
	}
	supersedes := store.snapshotSupersedes()
	if len(supersedes) != 1 {
		t.Fatalf("SupersedeFile ran %d times, want 1", len(supersedes))
	}
	if got := supersedes[0].head.FileId; got != "file-cut" {
		t.Errorf("the new head landed on file %q, want file-cut", got)
	}
	if got := store.linkStateOf("file-cut"); got != "synced" {
		t.Errorf("link state after a chunked re-push = %q, want synced", got)
	}
}
