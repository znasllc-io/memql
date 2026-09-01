package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/server/uploadsession"
)

// artifact_upload_sessions_test.go -- chunked resumable uploads and the
// streaming/Range download (memql#4782, design C1-C5).
//
// The claims, in the order the acceptance criteria state them:
//
//   - init opens a session with the quota and provenance checks in front of
//     it, and the refusals NAME THEIR NUMBERS;
//   - a chunk stages one bounded block; out-of-range n, oversize bodies,
//     foreign sessions and completed sessions are each refused with their
//     own status;
//   - the inventory lists exactly the staged chunk indexes -- what resume
//     reads;
//   - complete verifies staged bytes == declared bytes BEFORE committing,
//     refuses a mismatch with the session left open, commits in ascending
//     index order, creates the file row with sha256 ABSENT, and hands the
//     analysis pass a request with no Data (the pass streams the blob);
//   - ONE SESSION COMPLETES THROUGH TWO INDEPENDENT HANDLER INSTANCES. No
//     replica holds session state in memory; the multi-node rule holds by
//     construction, and this is the test that says so.
//   - the content route streams with Content-Length from the ROW, honours
//     a single-range Range as 206/Content-Range, answers 416 for an
//     unsatisfiable range, and ignores a malformed header (200, full body).

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeSessionStore models the real store's one load-bearing property: reads
// resolve under the caller's actor, so a session that is not yours is a
// session that is not there. Create stamps the owner from the context the
// way the @serverOnly mutation stamps it from the actor.
type fakeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*uploadsession.Row
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]*uploadsession.Row{}}
}

func (f *fakeSessionStore) Create(ctx context.Context, p uploadsession.CreateParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[p.UploadId] = &uploadsession.Row{
		ID: p.UploadId, OwnerUserId: actorUserId(ctx), Name: p.Name, Size: p.Size,
		MimeType: p.MimeType, FolderId: p.FolderId, Labels: append([]string(nil), p.Labels...),
		UploadedFromWorkerId: p.UploadedFromWorkerId, UploadedFromWorkerName: p.UploadedFromWorkerName,
		UploadedFromPath: p.UploadedFromPath, BlobPath: p.BlobPath, FileId: p.FileId,
		ChunkSize: p.ChunkSize, Status: "open",
	}
	return nil
}

func (f *fakeSessionStore) ByID(ctx context.Context, uploadId string) (*uploadsession.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.sessions[uploadId]
	if !ok || row.OwnerUserId != actorUserId(ctx) {
		return nil, nil
	}
	cp := *row
	return &cp, nil
}

func (f *fakeSessionStore) Complete(ctx context.Context, uploadId string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.sessions[uploadId]; ok {
		row.Status = "completed"
	}
	return nil
}

func (f *fakeSessionStore) statusOf(uploadId string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.sessions[uploadId]; ok {
		return row.Status
	}
	return ""
}

// fakeBlocks is the staged-block store: container/object -> blockID -> bytes.
type fakeBlocks struct {
	mu      sync.Mutex
	staged  map[string]map[string][]byte
	commits [][]string // ordered id lists, one per CommitBlockList call
}

func newFakeBlocks() *fakeBlocks { return &fakeBlocks{staged: map[string]map[string][]byte{}} }

func blocksKey(container, object string) string { return container + "/" + object }

func (b *fakeBlocks) StageBlock(_ context.Context, container, object, blockID string, chunk []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := blocksKey(container, object)
	if b.staged[key] == nil {
		b.staged[key] = map[string][]byte{}
	}
	b.staged[key][blockID] = append([]byte(nil), chunk...)
	return nil
}

func (b *fakeBlocks) CommitBlockList(_ context.Context, container, object string, blockIDs []string, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.commits = append(b.commits, append([]string(nil), blockIDs...))
	return nil
}

func (b *fakeBlocks) UncommittedBlocks(_ context.Context, container, object string) (map[string]int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int64{}
	for id, data := range b.staged[blocksKey(container, object)] {
		out[id] = int64(len(data))
	}
	return out, nil
}

func (b *fakeBlocks) commitCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.commits)
}

// fakeStreamer serves ranges over a named body, standing in for the Azure
// streaming client. Content comes from the map so the handler's headers can
// only be right by consulting the ROW, not the stream.
type fakeStreamer struct {
	objects map[string][]byte
}

func (s *fakeStreamer) DownloadStreamURL(_ context.Context, blobURL string) (io.ReadCloser, error) {
	data, ok := s.objects[blobURL]
	if !ok {
		return nil, fmt.Errorf("no such object: %s", blobURL)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *fakeStreamer) DownloadRangeURL(_ context.Context, blobURL string, offset, count int64) (io.ReadCloser, error) {
	data, ok := s.objects[blobURL]
	if !ok {
		return nil, fmt.Errorf("no such object: %s", blobURL)
	}
	if offset < 0 || offset >= int64(len(data)) || count <= 0 {
		return nil, fmt.Errorf("range out of bounds")
	}
	end := offset + count
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data[offset:end])), nil
}

// sessionsHandler builds an ArtifactHandler wired for chunked sessions with
// small deterministic budgets.
func sessionsHandler(store *fakeLibraryStore, sessions *fakeSessionStore, blocks *fakeBlocks, chunkSize int64, opts func(*ArtifactHandlerOptions)) *ArtifactHandler {
	o := ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b",
		Uploader: newFakeBlob(), Downloader: newFakeBlob(),
		Store: store, Sessions: sessions, Blocks: blocks,
		ChunkSizeBytes: chunkSize,
		PromotionWait:  10 * time.Millisecond, PromotionPoll: time.Millisecond,
	}
	if opts != nil {
		opts(&o)
	}
	return NewArtifactHandler(o)
}

func doJSON(t *testing.T, h http.Handler, method, path, userId string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req = req.WithContext(auth.ContextWithUserActor(req.Context(), userId))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func putChunk(t *testing.T, h http.Handler, userId, uploadId string, n int, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/artifacts/uploads/%s/chunks/%d", uploadId, n), bytes.NewReader(data))
	req = req.WithContext(auth.ContextWithUserActor(req.Context(), userId))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type initResponse struct {
	UploadId  string `json:"uploadId"`
	ChunkSize int64  `json:"chunkSize"`
}

func openSession(t *testing.T, h http.Handler, userId string, body map[string]any) initResponse {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/artifacts/uploads", userId, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("init status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var out initResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode init response %q: %v", rec.Body.String(), err)
	}
	if out.UploadId == "" || out.ChunkSize <= 0 {
		t.Fatalf("init response incomplete: %+v", out)
	}
	return out
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

func TestUploadInitOpensASessionWithVerifiedProvenance(t *testing.T) {
	store := newFakeLibraryStore()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	sessions := newFakeSessionStore()
	h := sessionsHandler(store, sessions, newFakeBlocks(), 4, nil)

	out := openSession(t, h, "user-a", map[string]any{
		"name": "big.mp4", "size": 10, "mimeType": "video/mp4",
		"folderId":             "fold-1",
		"labels":               []string{"raw"},
		"uploadedFromWorkerId": "wrk-1", "uploadedFromWorkerName": "form-name",
		"uploadedFromPath": "/Users/a/big.mp4",
	})
	if out.ChunkSize != 4 {
		t.Errorf("chunkSize = %d, want the handler's configured 4", out.ChunkSize)
	}
	row, err := sessions.ByID(auth.ContextWithUserActor(context.Background(), "user-a"), out.UploadId)
	if err != nil || row == nil {
		t.Fatalf("session row not readable by its owner: %v", err)
	}
	if row.UploadedFromWorkerName != "MacBook-Pro" {
		t.Errorf("session UploadedFromWorkerName = %q, want the REGISTRATION's name", row.UploadedFromWorkerName)
	}
	if row.FolderId != "fold-1" || len(row.Labels) != 1 || row.Labels[0] != "raw" {
		t.Errorf("init-time facts not recorded: %+v", row)
	}
	if row.FileId == "" || !strings.HasPrefix(row.BlobPath, "library/user-a/"+row.FileId+"/") {
		t.Errorf("BlobPath %q is not library/{userId}/{fileId}/{name} -- the path the concept documents", row.BlobPath)
	}
}

func TestUploadInitRefusesOverCapAndOverQuota(t *testing.T) {
	store := newFakeLibraryStore()
	sessions := newFakeSessionStore()
	h := sessionsHandler(store, sessions, newFakeBlocks(), 4, func(o *ArtifactHandlerOptions) {
		o.MaxUploadBytes = 100
		o.UserQuotaBytes = 150
	})

	// Over the per-file cap: 413, naming the knob.
	rec := doJSON(t, h, http.MethodPost, "/artifacts/uploads", "user-a", map[string]any{"name": "x", "size": 101})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap init = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), LibraryMaxUploadBytesEnv) {
		t.Errorf("cap refusal %q does not name the knob", rec.Body.String())
	}

	// Over the quota: stored bytes + OPEN SESSION bytes + this size. The
	// refusal names both numbers -- the would-be total and the quota.
	store.setFootprint(100, 30)
	rec = doJSON(t, h, http.MethodPost, "/artifacts/uploads", "user-a", map[string]any{"name": "x", "size": 40})
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-quota init = %d, want 507; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "170") || !strings.Contains(body, "150") {
		t.Errorf("quota refusal %q must name both numbers (the would-be total 170 and the quota 150)", body)
	}
	if !strings.Contains(body, LibraryUserQuotaBytesEnv) {
		t.Errorf("quota refusal %q does not name the knob", body)
	}

	// Under it -- 100 + 30 + 20 = 150, exactly at the quota -- is admitted.
	if rec := doJSON(t, h, http.MethodPost, "/artifacts/uploads", "user-a", map[string]any{"name": "x", "size": 20}); rec.Code != http.StatusCreated {
		t.Fatalf("at-quota init = %d, want 201 (the quota is a ceiling, not a strict bound); body: %s", rec.Code, rec.Body.String())
	}
}

func TestOneShotUploadAlsoPaysTheQuota(t *testing.T) {
	store := newFakeLibraryStore()
	store.setFootprint(100, 45)
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b", Uploader: blob, Downloader: blob,
		Store: store, UserQuotaBytes: 150,
		PromotionWait: 10 * time.Millisecond, PromotionPoll: time.Millisecond,
	})
	rec := postUpload(t, h, "user-a", "x.bin", "application/octet-stream", bytes.Repeat([]byte("a"), 10), nil)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("over-quota one-shot = %d, want 507; body: %s", rec.Code, rec.Body.String())
	}
	if n := len(store.snapshotCreated()); n != 0 {
		t.Errorf("createLibraryFile called %d times on a refused one-shot, want 0", n)
	}
	if n := blob.objectCount(); n != 0 {
		t.Errorf("%d objects reached storage on a refused one-shot, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// chunks + inventory
// ---------------------------------------------------------------------------

func TestUploadChunkStagesBoundedBlocksAndRefusesTheRest(t *testing.T) {
	store := newFakeLibraryStore()
	sessions := newFakeSessionStore()
	blocks := newFakeBlocks()
	h := sessionsHandler(store, sessions, blocks, 4, nil)

	out := openSession(t, h, "user-a", map[string]any{"name": "b.bin", "size": 10})

	// 10 bytes at chunk size 4 = chunks 1..3.
	if rec := putChunk(t, h, "user-a", out.UploadId, 1, []byte("aaaa")); rec.Code != http.StatusNoContent {
		t.Fatalf("chunk 1 = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	// Out of range n.
	if rec := putChunk(t, h, "user-a", out.UploadId, 4, []byte("x")); rec.Code != http.StatusBadRequest {
		t.Fatalf("chunk 4 of 3 = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	if rec := putChunk(t, h, "user-a", out.UploadId, 0, []byte("x")); rec.Code != http.StatusBadRequest {
		t.Fatalf("chunk 0 = %d, want 400", rec.Code)
	}
	// Oversize body.
	if rec := putChunk(t, h, "user-a", out.UploadId, 2, []byte("aaaaa")); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize chunk = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	// Empty body.
	if rec := putChunk(t, h, "user-a", out.UploadId, 2, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty chunk = %d, want 400", rec.Code)
	}
	// A second user's PUT: the session resolves under THEIR actor, finds
	// nothing, and the answer is the same 404 a missing session gets.
	if rec := putChunk(t, h, "user-b", out.UploadId, 2, []byte("aaaa")); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign chunk = %d, want 404", rec.Code)
	}

	// Inventory reflects exactly what is staged.
	rec := doJSON(t, h, http.MethodGet, "/artifacts/uploads/"+out.UploadId, "user-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory = %d; body: %s", rec.Code, rec.Body.String())
	}
	var inv struct {
		Status string `json:"status"`
		Staged []struct {
			N    int   `json:"n"`
			Size int64 `json:"size"`
		} `json:"staged"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatalf("decode inventory %q: %v", rec.Body.String(), err)
	}
	if inv.Status != "open" || len(inv.Staged) != 1 || inv.Staged[0].N != 1 || inv.Staged[0].Size != 4 {
		t.Fatalf("inventory = %+v, want exactly staged chunk 1 of 4 bytes on an open session", inv)
	}
	// And a second user cannot read it.
	if rec := doJSON(t, h, http.MethodGet, "/artifacts/uploads/"+out.UploadId, "user-b", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign inventory = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// complete
// ---------------------------------------------------------------------------

func TestUploadCompleteVerifiesCommitsAndCreatesTheRow(t *testing.T) {
	store := newFakeLibraryStore()
	withOwnedWorker(store, "user-a", "wrk-1", "MacBook-Pro")
	sessions := newFakeSessionStore()
	blocks := newFakeBlocks()
	analyzer := newFakeAnalyzer()
	h := sessionsHandler(store, sessions, blocks, 4, func(o *ArtifactHandlerOptions) {
		o.Analyzer = analyzer
	})

	out := openSession(t, h, "user-a", map[string]any{
		"name": "b.bin", "size": 10, "mimeType": "video/mp4",
		"folderId":             "fold-1",
		"labels":               []string{"raw", "q3"},
		"uploadedFromWorkerId": "wrk-1", "uploadedFromPath": "/Users/a/b.bin",
	})

	// Complete before all chunks are staged: refused, session stays open,
	// nothing committed, no row written, and the sentence names the bytes.
	putChunk(t, h, "user-a", out.UploadId, 1, []byte("aaaa"))
	rec := doJSON(t, h, http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "user-a", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("incomplete complete = %d, want 409; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "10") || !strings.Contains(rec.Body.String(), "4") {
		t.Errorf("mismatch refusal %q must name declared (10) and staged (4) bytes", rec.Body.String())
	}
	if blocks.commitCount() != 0 {
		t.Fatalf("a refused complete committed anyway")
	}
	if sessions.statusOf(out.UploadId) != "open" {
		t.Fatalf("a refused complete closed the session -- it must stay open for the client to finish")
	}
	if n := len(store.snapshotCreated()); n != 0 {
		t.Fatalf("a refused complete wrote %d file rows", n)
	}

	// Finish the chunks and complete for real.
	putChunk(t, h, "user-a", out.UploadId, 2, []byte("bbbb"))
	putChunk(t, h, "user-a", out.UploadId, 3, []byte("cc"))
	rec = doJSON(t, h, http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "user-a", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeUpload(t, &httptest.ResponseRecorder{Code: rec.Code, Body: rec.Body})
	if resp.FileId == "" || resp.ArtifactId == "" {
		t.Fatalf("complete response incomplete: %+v", resp)
	}

	// Committed in ascending index order.
	if blocks.commitCount() != 1 {
		t.Fatalf("commit called %d times, want 1", blocks.commitCount())
	}
	want := []string{uploadBlockID(1), uploadBlockID(2), uploadBlockID(3)}
	if got := blocks.commits[0]; !equalStrings(got, want) {
		t.Fatalf("committed order = %v, want %v -- blocks commit in ascending chunk order", got, want)
	}

	// The file row: sha256 ABSENT, everything else carried from the session.
	created := store.snapshotCreated()
	if len(created) != 1 {
		t.Fatalf("createLibraryFile called %d times, want 1", len(created))
	}
	p := created[0]
	if p.Sha256 != "" {
		t.Errorf("Sha256 = %q, want ABSENT -- the analysis pass stamps it by streaming the blob (D10)", p.Sha256)
	}
	if p.Size != 10 || p.FolderId != "fold-1" || p.UploadedFromWorkerId != "wrk-1" ||
		p.UploadedFromWorkerName != "MacBook-Pro" || p.UploadedFromPath != "/Users/a/b.bin" {
		t.Errorf("file row params lost session facts: %+v", p)
	}
	if p.BlobUrl == "" || !strings.Contains(p.BlobUrl, p.FileId) {
		t.Errorf("BlobUrl %q does not carry the session's blob path", p.BlobUrl)
	}

	// Labels went through the same builtin as everything else.
	if len(store.labels) != 2 {
		t.Errorf("labels applied = %v, want the session's two", store.labels)
	}

	// The analyzer got a request with NO data -- it streams the blob.
	select {
	case req := <-analyzer.calls:
		if req.Data != nil {
			t.Errorf("analysis request carries %d bytes of Data -- a chunked complete never held the file", len(req.Data))
		}
		if req.FileId != resp.FileId {
			t.Errorf("analysis request FileId = %q, want %q", req.FileId, resp.FileId)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the analysis pass was never handed the completed file")
	}

	if sessions.statusOf(out.UploadId) != "completed" {
		t.Fatalf("session status = %q after complete, want completed", sessions.statusOf(out.UploadId))
	}

	// Completing again answers the ids rather than re-running the commit --
	// the kill-after-complete-before-response resume case.
	rec = doJSON(t, h, http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "user-a", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-complete = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if blocks.commitCount() != 1 || len(store.snapshotCreated()) != 1 {
		t.Fatalf("re-complete re-ran the commit or the row write")
	}
}

func TestOneSessionCompletesThroughTwoIndependentHandlerInstances(t *testing.T) {
	// TWO handlers, ONE storage + graph. Nothing carried between them in
	// memory: the session row and the staged blocks are the whole state,
	// which is the multi-node rule holding by construction.
	store := newFakeLibraryStore()
	sessions := newFakeSessionStore()
	blocks := newFakeBlocks()
	a := sessionsHandler(store, sessions, blocks, 4, nil)
	b := sessionsHandler(store, sessions, blocks, 4, nil)

	out := openSession(t, a, "user-a", map[string]any{"name": "b.bin", "size": 8})
	putChunk(t, a, "user-a", out.UploadId, 1, []byte("aaaa"))
	// The second chunk lands on the OTHER replica.
	if rec := putChunk(t, b, "user-a", out.UploadId, 2, []byte("bbbb")); rec.Code != http.StatusNoContent {
		t.Fatalf("chunk 2 through instance B = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	// And the complete lands on B too.
	rec := doJSON(t, b, http.MethodPost, "/artifacts/uploads/"+out.UploadId+"/complete", "user-a", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("complete through instance B = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// block ids
// ---------------------------------------------------------------------------

func TestUploadBlockIDRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 42, 99999999} {
		id := uploadBlockID(n)
		got, ok := uploadBlockN(id)
		if !ok || got != n {
			t.Fatalf("block id round trip: n=%d -> %q -> (%d, %v)", n, id, got, ok)
		}
	}
	if _, ok := uploadBlockN("not-base64!"); ok {
		t.Fatalf("uploadBlockN accepted garbage")
	}
	// Fixed width: every id encodes to the same length, which is what Azure
	// requires of a block list (ids of differing lengths are refused).
	if len(uploadBlockID(1)) != len(uploadBlockID(99999999)) {
		t.Fatalf("block ids are not fixed-width")
	}
}

// ---------------------------------------------------------------------------
// streaming + Range
// ---------------------------------------------------------------------------

func rangeHandler(t *testing.T, body []byte) (*ArtifactHandler, string) {
	t.Helper()
	store := newFakeLibraryStore()
	artifactId := "art-1"
	fileRef := "v1:library:file:f-1"
	blobURL := "https://example.blob.core.windows.net/b/library/user-a/f-1/clip.mp4"
	store.artifacts[artifactId] = &LibraryArtifactRow{ID: artifactId, Kind: "file", SourceConceptRef: fileRef, Title: "clip.mp4"}
	store.files[fileRef] = &LibraryFileRow{ID: "f-1", Name: "clip.mp4", MimeType: "video/mp4", Size: len(body), BlobUrl: blobURL}
	store.ownerOf[artifactId] = "user-a"
	store.ownerOf[fileRef] = "user-a"
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "b", Store: store,
		Streamer: &fakeStreamer{objects: map[string][]byte{blobURL: body}},
	})
	return h, artifactId
}

func getContentWithRange(t *testing.T, h *ArtifactHandler, userId, artifactId, rangeHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/artifacts/"+artifactId+"/content", nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	req = req.WithContext(auth.ContextWithUserActor(req.Context(), userId))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestArtifactContentStreamsFullBodyWithRowLength(t *testing.T) {
	body := []byte("0123456789")
	h, artifactId := rangeHandler(t, body)

	rec := getContentWithRange(t, h, "user-a", artifactId, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != "10" {
		t.Errorf("Content-Length = %q, want 10 -- from the ROW, not from buffering", got)
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges missing -- a ranging client cannot discover the support")
	}
	if rec.Body.String() != "0123456789" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestArtifactContentHonoursASingleRange(t *testing.T) {
	body := []byte("0123456789")
	h, artifactId := rangeHandler(t, body)

	cases := []struct {
		header      string
		wantBody    string
		wantContent string
	}{
		{"bytes=2-5", "2345", "bytes 2-5/10"},
		{"bytes=7-", "789", "bytes 7-9/10"},
		{"bytes=-3", "789", "bytes 7-9/10"},
		{"bytes=0-0", "0", "bytes 0-0/10"},
		// An end past the size clamps, per RFC 9110.
		{"bytes=8-99", "89", "bytes 8-9/10"},
	}
	for _, c := range cases {
		rec := getContentWithRange(t, h, "user-a", artifactId, c.header)
		if rec.Code != http.StatusPartialContent {
			t.Fatalf("%s: status = %d, want 206; body: %s", c.header, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Range"); got != c.wantContent {
			t.Errorf("%s: Content-Range = %q, want %q", c.header, got, c.wantContent)
		}
		if rec.Body.String() != c.wantBody {
			t.Errorf("%s: body = %q, want %q", c.header, rec.Body.String(), c.wantBody)
		}
		if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(c.wantBody)) {
			t.Errorf("%s: Content-Length = %q, want %d", c.header, got, len(c.wantBody))
		}
	}
}

func TestArtifactContentRangeEdges(t *testing.T) {
	body := []byte("0123456789")
	h, artifactId := rangeHandler(t, body)

	// Unsatisfiable: start past the end.
	rec := getContentWithRange(t, h, "user-a", artifactId, "bytes=10-")
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("past-end range = %d, want 416; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */10" {
		t.Errorf("416 Content-Range = %q, want bytes */10", got)
	}

	// Malformed and multi-range headers are IGNORED (RFC 9110 lets a server
	// ignore Range), answering the full body rather than guessing.
	for _, header := range []string{"bytes=nonsense", "chunks=1-2", "bytes=1-2,4-5"} {
		rec := getContentWithRange(t, h, "user-a", artifactId, header)
		if rec.Code != http.StatusOK || rec.Body.String() != "0123456789" {
			t.Fatalf("%q: status=%d body=%q, want the full 200 -- an unparseable Range is ignored, never guessed at",
				header, rec.Code, rec.Body.String())
		}
	}
}
