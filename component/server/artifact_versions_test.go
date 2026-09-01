package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server/fileversion"
)

// artifact_versions_test.go -- the upload-new-version half of epic memql#4806.
//
// Three properties carry this feature, and each one has a test here that
// fails without it:
//
//   1. A VERSION IS NOT A SECOND ARTIFACT. The id, the filing and the labels
//      survive, the Files list still shows one row, and the outgoing head is
//      frozen -- with ITS OWN facts, not the new ones.
//   2. EVERY REFUSAL LANDS BEFORE A BYTE IS STORED. A foreign target, a
//      non-file target and a quota refusal must each leave storage and the
//      graph exactly as they were.
//   3. AN OLDER VERSION IS STILL DOWNLOADABLE, through the same
//      bearer-authenticated route, under the same 404-on-deny posture.

// seedVersionedFile seeds a file artifact whose head is at versionNumber, with
// its bytes in the blob store, and returns the canonical file ref.
func seedVersionedFile(store *fakeLibraryStore, blob *fakeBlob, owner, artifactId, fileId string,
	data []byte, mime, name string, version int) string {
	seedFileArtifact(store, blob, owner, artifactId, fileId, data, mime, name)
	ref := LibraryFileConceptRef(fileId)
	row := store.files[ref]
	row.VersionNumber = version
	row.VersionUploadedAt = "2026-08-01T10:00:00Z"
	row.Sha256 = strings.Repeat("aa", 32)
	row.Format = LibraryFormatForMIME(mime)
	row.Summary = "the first draft"
	return ref
}

func getContentVersion(t *testing.T, h *ArtifactHandler, userId, artifactId, version string) *httptest.ResponseRecorder {
	t.Helper()
	url := "/artifacts/" + artifactId + "/content"
	if version != "" {
		url += "?version=" + version
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(auth.ContextWithUserActor(req.Context(), userId))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A NEW VERSION KEEPS THE ARTIFACT. The response names the artifact the caller
// targeted -- not a new one -- and the head moves onto the new bytes while the
// OUTGOING version is frozen with its own name, hash, summary and moment.
func TestUploadNewVersionKeepsTheArtifactAndFreezesTheOutgoingHead(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("%PDF-1.7\nv1\n"), "application/pdf", "q3.pdf", 1)

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
		Analyzer: newFakeAnalyzer(),
	})

	rec := postUpload(t, h, "user-a", "q3-final.pdf", "application/pdf", []byte("%PDF-1.7\nv2 longer\n"),
		map[string]string{libraryFormTargetKey: "artifact-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	got := decodeUpload(t, rec)
	if got.ArtifactId != "artifact-1" {
		t.Errorf("artifactId = %q, want the target artifact -- a new version must not mint a second row", got.ArtifactId)
	}
	if got.FileId != "file-1" {
		t.Errorf("fileId = %q, want the existing file -- the head keeps its identity", got.FileId)
	}
	if got.VersionNumber != 2 {
		t.Errorf("versionNumber = %d, want 2", got.VersionNumber)
	}

	// NOTHING WAS CREATED. A CreateFile call here would be the second-artifact
	// bug wearing a different hat: a new file row promotes, and the Library
	// grows a duplicate.
	if created := store.snapshotCreated(); len(created) != 0 {
		t.Fatalf("a new version created %d file row(s); it must supersede the existing one", len(created))
	}

	calls := store.snapshotSupersedes()
	if len(calls) != 1 {
		t.Fatalf("recorded %d supersedes, want 1", len(calls))
	}
	snap, head := calls[0].snap, calls[0].head
	// THE SNAPSHOT IS THE OUTGOING VERSION, not the incoming one. Freezing
	// the new facts would rewrite history to say the old version was always
	// the new file.
	if snap.Name != "q3.pdf" || snap.VersionNumber != 1 {
		t.Errorf("snapshot froze %q at v%d, want the outgoing q3.pdf at v1", snap.Name, snap.VersionNumber)
	}
	if snap.Summary != "the first draft" {
		t.Errorf("snapshot summary = %q; a version's own summary is frozen with it", snap.Summary)
	}
	if snap.Sha256 != strings.Repeat("aa", 32) {
		t.Errorf("snapshot sha256 = %q, want the outgoing version's own hash", snap.Sha256)
	}
	if snap.UploadedAt != "2026-08-01T10:00:00Z" {
		t.Errorf("snapshot uploadedAt = %q; it must be when THOSE bytes arrived, not now", snap.UploadedAt)
	}
	if snap.VersionId != fileversion.DerivedVersionId("file-1", 1) {
		t.Errorf("snapshot id = %q, want the derived id so a retry cannot duplicate a version", snap.VersionId)
	}
	if head.VersionNumber != 2 || head.Name != "q3-final.pdf" {
		t.Errorf("head moved to %q at v%d, want q3-final.pdf at v2", head.Name, head.VersionNumber)
	}

	// THE BYTES LAND SOMEWHERE NO VERSION HAS BEEN. Overwriting the previous
	// version's object is the one thing this epic's durability invariant
	// forbids outright.
	if head.BlobUrl == snap.BlobUrl {
		t.Fatalf("the new version wrote to the previous version's path (%q) -- superseding must never touch stored bytes", head.BlobUrl)
	}
	if !strings.Contains(head.BlobUrl, "/file-1/") {
		t.Errorf("the new version's path %q does not sit under the file's own prefix", head.BlobUrl)
	}

	// The index is re-stamped, so the list shows the new name now rather than
	// whenever the analysis pass happens to finish.
	if len(store.restamped) != 1 || store.restamped[0] != "file-1" {
		t.Errorf("restamped = %v, want one re-stamp of file-1", store.restamped)
	}
}

// A VERSION OF SOMEBODY ELSE'S ARTIFACT IS REFUSED BEFORE ANY BYTE IS STORED.
// 404 rather than 403, for the same reason the download route answers 404:
// "not yours" and "not there" come back from the graph as one empty result.
func TestUploadNewVersionRefusesAForeignTargetBeforeStoringAnything(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("mine"), "text/plain", "mine.txt", 1)
	objectsBefore := len(blob.objects)

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})

	// The reachable positive: the OWNER's version upload works, so the
	// stranger's 404 is a refusal rather than a broken fixture.
	if rec := postUpload(t, h, "user-a", "v2.txt", "text/plain", []byte("theirs"),
		map[string]string{libraryFormTargetKey: "artifact-1"}); rec.Code != http.StatusCreated {
		t.Fatalf("the owner got %d; the fixture is broken and the negative below proves nothing. body: %s",
			rec.Code, rec.Body.String())
	}
	objectsAfterOwner := len(blob.objects)

	rec := postUpload(t, h, "user-b", "theirs.txt", "text/plain", []byte("not theirs"),
		map[string]string{libraryFormTargetKey: "artifact-1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger's version upload got %d, want 404. body: %s", rec.Code, rec.Body.String())
	}
	if len(blob.objects) != objectsAfterOwner {
		t.Fatalf("the refused upload stored %d new object(s); every refusal lands before a byte moves",
			len(blob.objects)-objectsAfterOwner)
	}
	if len(store.snapshotSupersedes()) != 1 {
		t.Fatal("the refused upload wrote a version row")
	}
	_ = objectsBefore
}

// VERSIONING A DOCUMENT IS REFUSED, and the sentence says where that flow
// lives. The person asking is not wrong, they are in the wrong place.
func TestUploadNewVersionRefusesANonFileKindAndSaysWhereToGo(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	store.artifacts["artifact-doc"] = &LibraryArtifactRow{
		ID: "artifact-doc", Kind: "document", SourceConceptRef: "v1:knowledge:document:d-1", Title: "notes",
	}
	store.ownerOf["artifact-doc"] = "user-a"

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})

	rec := postUpload(t, h, "user-a", "notes.md", "text/markdown", []byte("# notes"),
		map[string]string{libraryFormTargetKey: "artifact-doc"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "document") || !strings.Contains(body, "editing") {
		t.Errorf("the refusal does not name the flow that DOES version a document: %q", body)
	}
	if len(blob.objects) != 0 {
		t.Fatal("a refused non-file target still stored bytes")
	}
}

// THE QUOTA COUNTS EVERY VERSION, and the refusal says so. A person who can
// see one row per file has no way to reconcile a total that silently included
// their history.
func TestQuotaCountsSupersededVersionsAndTheRefusalSaysSo(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("head"), "text/plain", "a.txt", 3)
	store.setFootprint(10, 0)
	store.setVersionBytes(90)

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
		UserQuotaBytes: 100,
	})

	// 10 stored + 90 superseded is exactly the quota; one more byte is over.
	rec := postUpload(t, h, "user-a", "b.txt", "text/plain", []byte("x"), nil)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507 -- the superseded 90 bytes must count. body: %s",
			rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "101") || !strings.Contains(body, "100") {
		t.Errorf("the refusal names neither number: %q", body)
	}
	if !strings.Contains(body, "earlier version") {
		t.Errorf("the refusal does not say that earlier versions count, so the total cannot be reconciled: %q", body)
	}

	// The reachable positive: without the version bytes the same upload fits,
	// so the 507 above is about the history rather than about the fixture.
	store.setVersionBytes(0)
	if rec := postUpload(t, h, "user-a", "b.txt", "text/plain", []byte("x"), nil); rec.Code != http.StatusCreated {
		t.Fatalf("with no superseded bytes the upload got %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
}

// AN OLDER VERSION DOWNLOADS through the same route, and ?version= naming the
// CURRENT version serves the head unchanged.
func TestContentServesAnOlderVersionAndTheHead(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	v1 := []byte("first draft bytes")
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1", v1, "text/plain", "draft.txt", 1)

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
		Analyzer: newFakeAnalyzer(),
	})

	v2 := []byte("second draft, quite different")
	if rec := postUpload(t, h, "user-a", "draft.txt", "text/plain", v2,
		map[string]string{libraryFormTargetKey: "artifact-1"}); rec.Code != http.StatusCreated {
		t.Fatalf("the new version got %d. body: %s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		version string
		want    []byte
		label   string
	}{
		{"", v2, "no selector serves the head"},
		{"2", v2, "the head's own number serves the head"},
		{"1", v1, "an older number serves the older bytes"},
	}
	for _, tc := range cases {
		rec := getContentVersion(t, h, "user-a", "artifact-1", tc.version)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d. body: %s", tc.label, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != string(tc.want) {
			t.Errorf("%s: body = %q, want %q", tc.label, rec.Body.String(), string(tc.want))
		}
	}

	// The older version keeps its OWN filename in the download header, so
	// saving v1 does not silently produce a file named after v2.
	rec := getContentVersion(t, h, "user-a", "artifact-1", "1")
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "draft.txt") {
		t.Errorf("Content-Disposition = %q, want the version's own name", got)
	}
}

// A VERSION NOBODY HAS IS 404; A VERSION SELECTOR THAT IS NOT A NUMBER IS 400.
//
// The second half matters more than it looks: coercing an unreadable selector
// to the head would serve the NEWEST bytes under a 200 to somebody who asked
// for an old one, which is the one failure a download must never have.
func TestContentRefusesAnAbsentVersionAndAnUnreadableSelector(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("only version"), "text/plain", "a.txt", 1)

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})

	if rec := getContentVersion(t, h, "user-a", "artifact-1", "7"); rec.Code != http.StatusNotFound {
		t.Errorf("a version that does not exist got %d, want 404", rec.Code)
	}
	for _, bad := range []string{"0", "-1", "latest", "1.5"} {
		rec := getContentVersion(t, h, "user-a", "artifact-1", bad)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("version=%q got %d, want 400", bad, rec.Code)
			continue
		}
		if strings.Contains(rec.Body.String(), "only version") {
			t.Errorf("version=%q served the head's bytes under a refusal", bad)
		}
	}
}

// AN OLDER VERSION IS NOT VISIBLE TO A SECOND USER. The version read is
// owner-gated in its own right, so history cannot become a way around the
// artifact's own admission.
func TestOlderVersionsAreNotVisibleToASecondUser(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	v1 := []byte("private first draft")
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1", v1, "text/plain", "a.txt", 1)

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
		Analyzer: newFakeAnalyzer(),
	})
	if rec := postUpload(t, h, "user-a", "a.txt", "text/plain", []byte("second"),
		map[string]string{libraryFormTargetKey: "artifact-1"}); rec.Code != http.StatusCreated {
		t.Fatalf("seeding the second version got %d", rec.Code)
	}
	if rec := getContentVersion(t, h, "user-a", "artifact-1", "1"); rec.Code != http.StatusOK {
		t.Fatalf("the owner cannot read v1 (%d); the negative below would prove nothing", rec.Code)
	}

	rec := getContentVersion(t, h, "user-b", "artifact-1", "1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a second user got %d for an older version, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), string(v1)) {
		t.Fatal("a second user was served an older version's bytes")
	}
}

// A FILE ROW WITH NO versionNumber IS VERSION 1. Every file uploaded before
// this epic has no member at all, and reading that as 0 would freeze the
// outgoing head as "version 0" and renumber a file nobody touched.
func TestAPreVersionsFileSupersedesFromVersionOne(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	// seedFileArtifact, deliberately: it leaves versionNumber unset, which is
	// exactly the shape of every row written before the field existed.
	seedFileArtifact(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("old"), "text/plain", "old.txt")
	store.files[LibraryFileConceptRef("file-1")].VersionNumber = 0

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
		Analyzer: newFakeAnalyzer(),
	})
	rec := postUpload(t, h, "user-a", "new.txt", "text/plain", []byte("new"),
		map[string]string{libraryFormTargetKey: "artifact-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d. body: %s", rec.Code, rec.Body.String())
	}
	if got := decodeUpload(t, rec).VersionNumber; got != 2 {
		t.Errorf("versionNumber = %d, want 2 -- an absent number is version 1, never 0", got)
	}
	calls := store.snapshotSupersedes()
	if len(calls) != 1 || calls[0].snap.VersionNumber != 1 {
		t.Fatalf("froze the outgoing head at v%d, want v1", calls[0].snap.VersionNumber)
	}
}

// A FAILED SUPERSEDE LEAVES THE HEAD ALONE. The bytes are stored (they landed
// at a path nothing else uses, so they are inert), but the file must still
// hold the version it held, and the caller must be told.
func TestAFailedSupersedeAnswersAndLeavesTheHeadWhereItWas(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("head bytes"), "text/plain", "a.txt", 2)
	store.supersedeErr = fmt.Errorf("the version row would not write")

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})
	rec := postUpload(t, h, "user-a", "b.txt", "text/plain", []byte("new"),
		map[string]string{libraryFormTargetKey: "artifact-1"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500. body: %s", rec.Code, rec.Body.String())
	}
	if row := store.files[LibraryFileConceptRef("file-1")]; row.VersionNumber != 2 || row.Name != "a.txt" {
		t.Errorf("the head moved to %q at v%d despite the failure", row.Name, row.VersionNumber)
	}
	if got := getContentVersion(t, h, "user-a", "artifact-1", ""); got.Body.String() != "head bytes" {
		t.Errorf("the head serves %q; a failed supersede must change nothing a reader can see", got.Body.String())
	}
}

// STORAGE FAILING ON A SUPERSEDE MUST NOT TOUCH THE FILE. A fresh upload
// writes its row and marks it failed -- the owner has nothing else to look at
// -- but a supersede has a perfectly good previous version, and marking THAT
// failed would take a working file away over an upload that never landed.
func TestASupersedeWhoseBytesDidNotLandLeavesTheFileWorking(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("still here"), "text/plain", "a.txt", 1)

	// No Uploader: storage is not configured on this node.
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Downloader: blob, Store: store,
	})
	rec := postUpload(t, h, "user-a", "b.txt", "text/plain", []byte("new"),
		map[string]string{libraryFormTargetKey: "artifact-1"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body: %s", rec.Code, rec.Body.String())
	}
	if statuses := store.snapshotStatuses(); len(statuses) != 0 {
		t.Errorf("the supersede marked the existing file %v; the previous version is fine and stays fine", statuses)
	}
	if len(store.snapshotSupersedes()) != 0 {
		t.Error("the head moved even though no bytes were stored")
	}
}

// ---------------------------------------------------------------------------
// the chunked half
// ---------------------------------------------------------------------------

// A CHUNKED NEW VERSION supersedes at complete, and the target is gated at
// INIT -- before a single chunk is staged. Fail-fast is the whole reason the
// gate lives there rather than at complete: the alternative is discovering a
// foreign target after somebody has streamed gigabytes.
func TestChunkedNewVersionGatesAtInitAndSupersedesAtComplete(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("v1"), "video/mp4", "clip.mp4", 1)
	sessions := newFakeSessionStore()
	blocks := newFakeBlocks()
	analyzer := newFakeAnalyzer()
	h := sessionsHandler(store, sessions, blocks, 4, func(o *ArtifactHandlerOptions) {
		o.Analyzer = analyzer
	})

	// A stranger cannot even OPEN a session against somebody else's artifact.
	if rec := doJSON(t, h, http.MethodPost, "/artifacts/uploads", "user-b", map[string]any{
		"name": "clip.mp4", "size": 10, "mimeType": "video/mp4", "targetArtifactId": "artifact-1",
	}); rec.Code != http.StatusNotFound {
		t.Fatalf("a stranger's version session opened with %d, want 404. body: %s", rec.Code, rec.Body.String())
	}
	if len(blocks.staged) != 0 {
		t.Fatal("a refused init staged blocks")
	}

	out := openSession(t, h, "user-a", map[string]any{
		"name": "clip-v2.mp4", "size": 10, "mimeType": "video/mp4", "targetArtifactId": "artifact-1",
	})
	putChunk(t, h, "user-a", out.UploadId, 1, []byte("aaaa"))
	putChunk(t, h, "user-a", out.UploadId, 2, []byte("bbbb"))
	putChunk(t, h, "user-a", out.UploadId, 3, []byte("cc"))

	rec := doJSON(t, h, http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "user-a", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	got := decodeUpload(t, rec)
	if got.ArtifactId != "artifact-1" || got.FileId != "file-1" || got.VersionNumber != 2 {
		t.Errorf("complete answered %+v, want artifact-1 / file-1 / v2", got)
	}
	if n := len(store.snapshotCreated()); n != 0 {
		t.Fatalf("a chunked version created %d file row(s); it must supersede", n)
	}
	calls := store.snapshotSupersedes()
	if len(calls) != 1 {
		t.Fatalf("recorded %d supersedes, want 1", len(calls))
	}
	// THE HASH IS BLANK AND WRITTEN AS SUCH. No handler on this path ever
	// held the file, so inheriting the previous version's hash would be a
	// false integrity claim on bytes it does not describe.
	if calls[0].head.Sha256 != "" {
		t.Errorf("the chunked head carries sha256 %q; it must be blank until the analysis pass measures it",
			calls[0].head.Sha256)
	}
	if calls[0].snap.Sha256 == "" {
		t.Error("the snapshot lost the outgoing version's hash, which WAS measured")
	}
	// The re-complete is idempotent and still names the target.
	again := doJSON(t, h, http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "user-a", nil)
	if again.Code != http.StatusOK {
		t.Fatalf("re-complete = %d, want 200", again.Code)
	}
	if id := decodeUpload(t, again).ArtifactId; id != "artifact-1" {
		t.Errorf("re-complete answered artifact %q, want the target", id)
	}
	if len(store.snapshotSupersedes()) != 1 {
		t.Error("the re-complete superseded a second time")
	}
}

// A CHUNKED VERSION SESSION STAGES UNDER THE EXISTING FILE, at a path no
// version has used. Two things ride on this: the file's bytes stay together
// under one prefix, and the previous version's object is untouchable.
func TestChunkedVersionSessionStagesAtAFreshPathUnderTheSameFile(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedVersionedFile(store, blob, "user-a", "artifact-1", "file-1",
		[]byte("v1"), "text/plain", "a.txt", 1)
	sessions := newFakeSessionStore()
	blocks := newFakeBlocks()
	h := sessionsHandler(store, sessions, blocks, 4, nil)

	out := openSession(t, h, "user-a", map[string]any{
		"name": "a.txt", "size": 4, "mimeType": "text/plain", "targetArtifactId": "artifact-1",
	})
	row, err := sessions.ByID(auth.ContextWithUserActor(t.Context(), "user-a"), out.UploadId)
	if err != nil || row == nil {
		t.Fatalf("session lookup: %v", err)
	}
	if row.FileId != "file-1" {
		t.Errorf("session fileId = %q, want the EXISTING file -- a version keeps its identity", row.FileId)
	}
	if row.TargetArtifactId != "artifact-1" {
		t.Errorf("session targetArtifactId = %q; complete cannot re-ask the client which artifact it may write to", row.TargetArtifactId)
	}
	if !strings.Contains(row.BlobPath, "library/user-a/file-1/") {
		t.Errorf("staging path %q does not sit under the file's own prefix", row.BlobPath)
	}
	if strings.HasSuffix(row.BlobPath, "library/user-a/file-1/a.txt") {
		t.Errorf("staging path %q is the FIRST version's path -- a version must never stage over stored bytes", row.BlobPath)
	}
}
