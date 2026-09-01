package server

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func time10ms() time.Duration { return 10 * time.Millisecond }
func time1ms() time.Duration  { return time.Millisecond }

// artifact_handler_provenance_test.go -- folder filing + verified upload
// provenance on the one-shot route (memql#4781, design B2/B3/D5).
//
// Three claims:
//
//  1. A caller naming a worker registration THEY OWN gets the trio stamped
//     onto createLibraryFile -- with the NAME resolved from the registration
//     rather than taken on faith from the form, so the label the Files app
//     renders is the label the fleet already shows.
//  2. A caller naming a registration that is NOT theirs is REFUSED, before
//     any byte reaches storage and before any row is written. A
//     silently-dropped claim would render as "uploaded here", which is a
//     lie; a row written before the refusal would be an upload the response
//     says did not happen.
//  3. A browser upload -- no provenance fields at all -- never pays the
//     fleet read, and stamps nothing. The absence of a claim is not a claim.
//
// The fake gates OwnedWorker on the ACTOR, the same way its sibling reads
// are gated, because that is the property the real store gets from running
// myWorkersWithStatus under the caller's own actor.

func withOwnedWorker(f *fakeLibraryStore, owner, workerId, name string) {
	f.workers[workerId] = &LibraryWorkerRef{ID: workerId, Name: name}
	f.ownerOf["worker:"+workerId] = owner
}

func TestArtifactUploadStampsFolderAndVerifiedProvenance(t *testing.T) {
	store := newFakeLibraryStore()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b", Uploader: blob, Downloader: blob,
		Store: store, PromotionWait: time10ms(), PromotionPoll: time1ms(),
	})

	rec := postUpload(t, h, "user-a", "q3.pdf", "application/pdf", []byte("%PDF-1.7 x"), map[string]string{
		"folderId":               "fold-9",
		"uploadedFromWorkerId":   "wrk-1",
		"uploadedFromWorkerName": "a-name-the-form-made-up",
		"uploadedFromPath":       "/Users/a/Reports/q3.pdf",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}

	created := store.snapshotCreated()
	if len(created) != 1 {
		t.Fatalf("createLibraryFile called %d times, want 1", len(created))
	}
	p := created[0]
	if p.FolderId != "fold-9" {
		t.Errorf("FolderId = %q, want fold-9 -- the upload route is the writer that knows the target folder", p.FolderId)
	}
	if p.UploadedFromWorkerId != "wrk-1" {
		t.Errorf("UploadedFromWorkerId = %q, want wrk-1", p.UploadedFromWorkerId)
	}
	if p.UploadedFromWorkerName != "MacBook-Pro" {
		t.Errorf("UploadedFromWorkerName = %q, want the REGISTRATION's own name (MacBook-Pro), not the form's -- "+
			"the label is resolved from the verified row, never taken on faith", p.UploadedFromWorkerName)
	}
	if p.UploadedFromPath != "/Users/a/Reports/q3.pdf" {
		t.Errorf("UploadedFromPath = %q, want the reported path", p.UploadedFromPath)
	}
}

func TestArtifactUploadRefusesAForeignWorkerClaim(t *testing.T) {
	store := newFakeLibraryStore()
	// The registration exists -- but it is user-b's machine.
	withOwnedWorker(store, "user-b", "wrk-2", "Other-Machine")
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b", Uploader: blob, Downloader: blob,
		Store: store, PromotionWait: time10ms(), PromotionPoll: time1ms(),
	})

	rec := postUpload(t, h, "user-a", "q3.pdf", "application/pdf", []byte("%PDF-1.7 x"), map[string]string{
		"uploadedFromWorkerId": "wrk-2",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 -- a provenance claim that fails verification REFUSES the upload; body: %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "wrk-2") {
		t.Errorf("refusal %q does not name the claimed registration -- the sentence renders verbatim in the OS and must say which claim failed", rec.Body.String())
	}
	if n := len(store.snapshotCreated()); n != 0 {
		t.Errorf("createLibraryFile called %d times after a refused claim, want 0 -- a row written before the refusal is an upload the response denies", n)
	}
	if n := blob.objectCount(); n != 0 {
		t.Errorf("%d objects reached storage after a refused claim, want 0 -- verification happens before any byte is stored", n)
	}
}

func TestArtifactUploadWithAPathButNoMachineIsRefused(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b", Uploader: blob, Downloader: blob,
		Store: store, PromotionWait: time10ms(), PromotionPoll: time1ms(),
	})

	rec := postUpload(t, h, "user-a", "q3.pdf", "application/pdf", []byte("%PDF-1.7 x"), map[string]string{
		"uploadedFromPath": "/Users/a/Reports/q3.pdf",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 -- a path with no machine is half a provenance claim, and nothing can verify it; body: %s",
			rec.Code, rec.Body.String())
	}
	if n := len(store.snapshotCreated()); n != 0 {
		t.Errorf("createLibraryFile called %d times, want 0", n)
	}
}

func TestArtifactUploadWithoutProvenanceNeverReadsTheFleet(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b", Uploader: blob, Downloader: blob,
		Store: store, PromotionWait: time10ms(), PromotionPoll: time1ms(),
	})

	rec := postUpload(t, h, "user-a", "plain.txt", "text/plain", []byte("hello"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	if n := store.ownedWorkerCalls(); n != 0 {
		t.Errorf("OwnedWorker read %d times for an upload with no claim, want 0 -- a browser upload must not pay a fleet read", n)
	}
	created := store.snapshotCreated()
	if len(created) != 1 {
		t.Fatalf("createLibraryFile called %d times, want 1", len(created))
	}
	p := created[0]
	if p.FolderId != "" || p.UploadedFromWorkerId != "" || p.UploadedFromWorkerName != "" || p.UploadedFromPath != "" {
		t.Errorf("provenance stamped with no claim: %+v -- the absence of a claim is not a claim", p)
	}
}
