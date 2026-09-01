package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// artifact_handler_test.go -- the Library's two byte-bearing routes
// (memql#4341), covered at the two levels that catch different failures.
//
//  1. HANDLER BEHAVIOUR against a fake store: three MIME types including an
//     unknown one, the cap, a second user's download, a note exporting as
//     markdown, and a missing uploader recording a `failed` row rather than a
//     `local://` placeholder that pretends. The fake gates its reads on the
//     ACTOR in the context, because that is the property the real store gets
//     from the engine's per-row authorization and a fake that answered every
//     caller would make the cross-user test vacuous.
//
//  2. THE RENDERED CALL SITES against the REAL front end. A handler suite that
//     records query strings and never parses them is how whole features ship
//     failing at parse (memql#4256, #1454), and this file's own store hit that
//     trap while it was being written: libraryAddArtifactLabel is a BUILTIN and
//     refuses the bare named-args form every other call site uses. The engine
//     is what says so, so the engine is what the test asks.

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeLibraryStore answers from in-memory maps, gating every read on the
// caller's actor. ownerOf is what makes it a model of the real thing: the
// production store's reads run owner-gated in the engine, so "not yours" and
// "not there" arrive as the same empty result.
type fakeLibraryStore struct {
	mu sync.Mutex

	created  []LibraryFileCreateParams
	statuses []LibraryFileStatusParams
	labels   []string // "<artifactId>=<label>"

	artifacts map[string]*LibraryArtifactRow
	files     map[string]*LibraryFileRow
	bodies    map[string]*LibraryExportBody
	ownerOf   map[string]string // artifact id / file ref / body ref / "worker:<id>" -> owner user id

	// workers answers OwnedWorker, gated on the actor like every other read
	// (memql#4781): the real store runs myWorkersWithStatus under the
	// caller's own actor, so "not yours" and "not there" are one empty
	// answer. ownedWorkerN counts the reads so a test can assert an upload
	// with no claim never pays one.
	workers      map[string]*LibraryWorkerRef
	ownedWorkerN int

	// artifactForFile answers ArtifactForFile, but only after promoteAfter
	// calls -- the promotion is an automation off graph.node.created, so the
	// first read genuinely can miss.
	artifactForFile  map[string]string
	promoteAfter     int
	artifactForFileN int

	createErr error
}

func newFakeLibraryStore() *fakeLibraryStore {
	return &fakeLibraryStore{
		artifacts:       map[string]*LibraryArtifactRow{},
		files:           map[string]*LibraryFileRow{},
		bodies:          map[string]*LibraryExportBody{},
		ownerOf:         map[string]string{},
		artifactForFile: map[string]string{},
		workers:         map[string]*LibraryWorkerRef{},
	}
}

func (f *fakeLibraryStore) OwnedWorker(ctx context.Context, workerId string) (*LibraryWorkerRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownedWorkerN++
	if !f.admits(ctx, "worker:"+workerId) {
		return nil, nil
	}
	return f.workers[workerId], nil
}

func (f *fakeLibraryStore) ownedWorkerCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ownedWorkerN
}

func actorUserId(ctx context.Context) string {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		return strings.TrimSpace(ac.UserId)
	}
	return ""
}

func (f *fakeLibraryStore) admits(ctx context.Context, key string) bool {
	owner, ok := f.ownerOf[key]
	return ok && owner == actorUserId(ctx)
}

func (f *fakeLibraryStore) CreateFile(_ context.Context, p LibraryFileCreateParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, p)
	return nil
}

func (f *fakeLibraryStore) SetFileStatus(_ context.Context, p LibraryFileStatusParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, p)
	return nil
}

func (f *fakeLibraryStore) ArtifactForFile(_ context.Context, fileId string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.artifactForFileN++
	if f.artifactForFileN <= f.promoteAfter {
		return "", nil
	}
	if id, ok := f.artifactForFile[fileId]; ok {
		return id, nil
	}
	// Default: any file promotes to a derived-looking id. The DERIVATION is
	// the DSL's; this is a stand-in for "the automation landed", not a copy of
	// concat("artifact-", hash(ref)).
	return "artifact-for-" + fileId, nil
}

func (f *fakeLibraryStore) AddArtifactLabel(_ context.Context, artifactId, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels = append(f.labels, artifactId+"="+label)
	return nil
}

func (f *fakeLibraryStore) Artifact(ctx context.Context, artifactId string) (*LibraryArtifactRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.admits(ctx, artifactId) {
		return nil, nil
	}
	return f.artifacts[artifactId], nil
}

func (f *fakeLibraryStore) File(ctx context.Context, fileRef string) (*LibraryFileRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.admits(ctx, fileRef) {
		return nil, nil
	}
	return f.files[fileRef], nil
}

func (f *fakeLibraryStore) ExportBody(ctx context.Context, kind, ref string) (*LibraryExportBody, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case "note", "generated_output", "memory":
	default:
		return nil, nil
	}
	if !f.admits(ctx, ref) {
		return nil, nil
	}
	return f.bodies[ref], nil
}

func (f *fakeLibraryStore) snapshotCreated() []LibraryFileCreateParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LibraryFileCreateParams(nil), f.created...)
}

func (f *fakeLibraryStore) snapshotStatuses() []LibraryFileStatusParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LibraryFileStatusParams(nil), f.statuses...)
}

// fakeBlob is both the uploader and the downloader, the way the real Azure
// client is one value satisfying both interfaces.
type fakeBlob struct {
	mu      sync.Mutex
	objects map[string][]byte
	upErr   error
}

func newFakeBlob() *fakeBlob { return &fakeBlob{objects: map[string][]byte{}} }

func (b *fakeBlob) Upload(_ context.Context, container, objectName string, data []byte, _ string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.upErr != nil {
		return "", b.upErr
	}
	url := "https://example.blob.core.windows.net/" + container + "/" + objectName
	b.objects[url] = append([]byte(nil), data...)
	return url, nil
}

func (b *fakeBlob) objectCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.objects)
}

func (b *fakeBlob) DownloadURL(_ context.Context, blobURL string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[blobURL]
	if !ok {
		return nil, fmt.Errorf("no such object: %s", blobURL)
	}
	return data, nil
}

// fakeAnalyzer records the hand-off. Buffered so the detached goroutine never
// blocks and the test never has to sleep to observe it.
type fakeAnalyzer struct{ calls chan LibraryAnalysisRequest }

func newFakeAnalyzer() *fakeAnalyzer {
	return &fakeAnalyzer{calls: make(chan LibraryAnalysisRequest, 8)}
}

func (a *fakeAnalyzer) AnalyzeFile(_ context.Context, req LibraryAnalysisRequest) {
	a.calls <- req
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ---------------------------------------------------------------------------
// request helpers
// ---------------------------------------------------------------------------

// uploadBody builds a multipart body with an EXPLICIT part Content-Type when
// one is given, and none at all when it is empty -- multipart.CreateFormFile
// always stamps application/octet-stream, which would make "the client sent no
// type, so sniff" untestable.
func uploadBody(t *testing.T, fileName, contentType string, data []byte, fields map[string]string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, libraryFormFileKey, fileName))
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func postUpload(t *testing.T, h *ArtifactHandler, userId, fileName, contentType string, data []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := uploadBody(t, fileName, contentType, data, fields)
	req := httptest.NewRequest(http.MethodPost, "/artifacts", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.ContextWithUserActor(req.Context(), userId))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func getContent(t *testing.T, h *ArtifactHandler, userId, artifactId string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/artifacts/"+artifactId+"/content", nil)
	req = req.WithContext(auth.ContextWithUserActor(req.Context(), userId))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeUpload(t *testing.T, rec *httptest.ResponseRecorder) ArtifactUploadResponse {
	t.Helper()
	var out ArtifactUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode upload response %q: %v", rec.Body.String(), err)
	}
	return out
}

// ---------------------------------------------------------------------------
// upload
// ---------------------------------------------------------------------------

// THREE MIME TYPES INCLUDING AN UNKNOWN ONE (design 6, item 1).
//
// The unknown one is the case with its own behaviour rather than just another
// row: any MIME is accepted (there is no allowlist -- one of the reasons the
// Library does not ride the attachment routes), it classifies as `other`, and
// it goes STRAIGHT to ready with nothing analyzed. A test that only uploaded
// three recognised types would assert the easy half.
func TestArtifactUploadAcceptsThreeMIMETypesIncludingAnUnknownOne(t *testing.T) {
	cases := []struct {
		name        string
		fileName    string
		contentType string
		data        []byte
		wantMIME    string
		wantFormat  string
		wantOpaque  bool
	}{
		{
			name: "markdown", fileName: "notes.md", contentType: "text/markdown; charset=utf-8",
			data: []byte("# hello\n"), wantMIME: "text/markdown", wantFormat: "markdown",
		},
		{
			name: "pdf", fileName: "report.pdf", contentType: "application/pdf",
			data: []byte("%PDF-1.7\n%fake\n"), wantMIME: "application/pdf", wantFormat: "pdf",
		},
		{
			// A type nothing recognises. Stored opaquely, never refused.
			name: "unknown", fileName: "model.bin", contentType: "application/x-memql-unknown",
			data: []byte{0x00, 0x01, 0x02, 0x03}, wantMIME: "application/x-memql-unknown",
			wantFormat: libraryFormatOther, wantOpaque: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeLibraryStore()
			blob := newFakeBlob()
			analyzer := newFakeAnalyzer()
			h := NewArtifactHandler(ArtifactHandlerOptions{
				Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
				Store: store, Analyzer: analyzer, PromotionWait: 50 * time.Millisecond,
			})

			rec := postUpload(t, h, "user-a", tc.fileName, tc.contentType, tc.data, nil)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
			}
			resp := decodeUpload(t, rec)
			if resp.FileId == "" {
				t.Error("201 carried no fileId")
			}
			if resp.ArtifactId == "" {
				t.Error("201 carried no artifactId; the promotion should have resolved")
			}

			created := store.snapshotCreated()
			if len(created) != 1 {
				t.Fatalf("createLibraryFile called %d times, want 1", len(created))
			}
			got := created[0]
			if got.MimeType != tc.wantMIME {
				t.Errorf("mimeType = %q, want %q", got.MimeType, tc.wantMIME)
			}
			if got.Format != tc.wantFormat {
				t.Errorf("format = %q, want %q", got.Format, tc.wantFormat)
			}
			if got.Size != len(tc.data) {
				t.Errorf("size = %d, want %d", got.Size, len(tc.data))
			}
			if got.Source != "uploaded" {
				t.Errorf("source = %q, want %q", got.Source, "uploaded")
			}
			if len(got.Sha256) != 64 {
				t.Errorf("sha256 = %q, want 64 hex characters", got.Sha256)
			}
			if want := fmt.Sprintf("library/%s/%s/%s", "user-a", got.FileId, tc.fileName); !strings.HasSuffix(got.BlobUrl, want) {
				t.Errorf("blobUrl = %q, want it to end in the documented storage path %q", got.BlobUrl, want)
			}

			// The opaque case closes its own lifecycle; the recognised ones
			// hand off and let memql#4342 close theirs.
			statuses := store.snapshotStatuses()
			if tc.wantOpaque {
				if len(statuses) != 1 || statuses[0].Status != libraryStatusReady {
					t.Fatalf("an unrecognised type must go straight to ready; statuses = %+v", statuses)
				}
				select {
				case req := <-analyzer.calls:
					t.Fatalf("the analyzer was called for an opaque type: %+v", req)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
			if len(statuses) != 0 {
				t.Errorf("the handler wrote a status for an analyzable type; that belongs to the "+
					"analysis pass: %+v", statuses)
			}
			select {
			case req := <-analyzer.calls:
				if req.FileId != got.FileId || req.OwnerUserId != "user-a" ||
					req.Format != tc.wantFormat || string(req.Data) != string(tc.data) {
					t.Errorf("analysis request = %+v, want the file's own ids, owner, format and bytes", req)
				}
				if req.ArtifactId == "" {
					t.Error("analysis request carries no artifactId; chunks need it")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the analyzer was never called for an analyzable type")
			}
		})
	}
}

// The client sent no part Content-Type at all, so the bytes decide.
func TestArtifactUploadSniffsAnAbsentContentType(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, PromotionWait: 50 * time.Millisecond,
	})

	rec := postUpload(t, h, "user-a", "plain.txt", "", []byte("just some words\n"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	created := store.snapshotCreated()
	if len(created) != 1 || created[0].MimeType != "text/plain" {
		t.Fatalf("mimeType = %q, want text/plain from sniffing", created[0].MimeType)
	}
}

// The name and labels form fields, both optional.
func TestArtifactUploadHonoursTheNameAndLabelFields(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, PromotionWait: 50 * time.Millisecond,
	})

	rec := postUpload(t, h, "user-a", "part-filename.md", "text/markdown", []byte("x"),
		map[string]string{
			libraryFormNameKey:   "chosen name.md",
			libraryFormLabelsKey: "alpha, beta ,, alpha",
		})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body.String())
	}
	created := store.snapshotCreated()
	if created[0].Name != "chosen name.md" {
		t.Errorf("name = %q, want the `name` field to win over the part filename", created[0].Name)
	}
	artifactId := decodeUpload(t, rec).ArtifactId
	want := []string{artifactId + "=alpha", artifactId + "=beta"}
	if strings.Join(store.labels, "|") != strings.Join(want, "|") {
		t.Errorf("labels = %v, want %v (blank entries dropped, duplicates collapsed)", store.labels, want)
	}
}

// THE CAP (design 6, item 1). 413, not 400 -- the multipart parser reports
// MaxBytesReader's refusal as its own parse error, and reading that as
// "malformed form" is how an oversized upload comes back as a client syntax
// problem.
func TestArtifactUploadRefusesOverTheCap(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, MaxUploadBytes: 1024, PromotionWait: 50 * time.Millisecond,
	})

	rec := postUpload(t, h, "user-a", "big.bin", "application/octet-stream",
		bytes.Repeat([]byte("x"), 4096), nil)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413. body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), LibraryMaxUploadBytesEnv) {
		t.Errorf("the 413 must name %s so an operator can find the knob; got %q",
			LibraryMaxUploadBytesEnv, rec.Body.String())
	}
	if len(store.snapshotCreated()) != 0 {
		t.Error("a refused upload wrote a row")
	}
	if len(blob.objects) != 0 {
		t.Error("a refused upload wrote bytes to storage")
	}
}

// THE OTHER 413 PATH, and the subtle one. The test above trips the
// header.Size check, which reports the refusal itself. A body big enough to
// exhaust MaxBytesReader trips inside the MULTIPART PARSER, which wraps the
// read error into its own -- and reading that as "malformed form" is a 400 for
// what is a 413, which is precisely why isRequestTooLarge exists. Both paths
// have to answer 413 or the enforcement is only half wired.
func TestArtifactUploadRefusesABodyThatExhaustsTheReader(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, MaxUploadBytes: 1024, PromotionWait: 50 * time.Millisecond,
	})

	// Past the cap AND past the framing allowance, so the reader gives out
	// before the parser ever sees a complete part.
	rec := postUpload(t, h, "user-a", "huge.bin", "application/octet-stream",
		bytes.Repeat([]byte("x"), 1024+libraryUploadFramingAllowance+4096), nil)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (a 400 here means the MaxBytesReader refusal was read as "+
			"a malformed form). body: %s", rec.Code, rec.Body.String())
	}
	if len(store.snapshotCreated()) != 0 {
		t.Error("a refused upload wrote a row")
	}
}

// A body UNDER the cap is not refused -- the negative of the test above, so a
// cap that refused everything could not pass both.
func TestArtifactUploadAcceptsRightUpToTheCap(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob,
		Store: store, MaxUploadBytes: 1024, PromotionWait: 50 * time.Millisecond,
	})

	rec := postUpload(t, h, "user-a", "small.bin", "application/octet-stream",
		bytes.Repeat([]byte("x"), 1024), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a body exactly at the cap. body: %s", rec.Code, rec.Body.String())
	}
}

func TestLibraryMaxUploadBytesReadsTheEnvironment(t *testing.T) {
	if got := LibraryMaxUploadBytes(); got != DefaultLibraryMaxUploadBytes {
		t.Fatalf("unset: got %d, want the %d default", got, DefaultLibraryMaxUploadBytes)
	}
	t.Setenv(LibraryMaxUploadBytesEnv, "12345")
	if got := LibraryMaxUploadBytes(); got != 12345 {
		t.Errorf("set: got %d, want 12345", got)
	}
	// A misconfigured cap falls back to the default, never to "no limit".
	for _, bad := range []string{"", "not-a-number", "0", "-1"} {
		t.Setenv(LibraryMaxUploadBytesEnv, bad)
		if got := LibraryMaxUploadBytes(); got != DefaultLibraryMaxUploadBytes {
			t.Errorf("%q: got %d, want the %d default", bad, got, DefaultLibraryMaxUploadBytes)
		}
	}
}

func TestArtifactUploadRefusesAnUnauthenticatedCaller(t *testing.T) {
	store := newFakeLibraryStore()
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: store})

	body, ct := uploadBody(t, "x.md", "text/markdown", []byte("x"), nil)
	req := httptest.NewRequest(http.MethodPost, "/artifacts", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body: %s", rec.Code, rec.Body.String())
	}
	if len(store.snapshotCreated()) != 0 {
		t.Error("an unauthenticated upload wrote a row")
	}
}

// A MISSING UPLOADER YIELDS A `failed` ROW WITH A REASON (design 6, item 1).
//
// The attachment handler's degraded shape -- write `local://<path>` and call
// the row `processing` -- is what this must not do. That row reads to every
// consumer as a stored file, and the person who uploaded it has nothing to
// look at. Here the row exists (so the failure is VISIBLE where the owner
// looks) and says failed, with a reason on the row rather than only in a log,
// and the response is not a 201.
func TestArtifactUploadWithNoUploaderRecordsAFailedRowWithAReason(t *testing.T) {
	store := newFakeLibraryStore()
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Store: store, PromotionWait: 50 * time.Millisecond,
	}) // no Uploader, no Bucket

	rec := postUpload(t, h, "user-a", "notes.md", "text/markdown", []byte("# hi"), nil)

	if rec.Code == http.StatusCreated {
		t.Fatalf("status = 201 for an upload whose bytes were never stored. body: %s", rec.Body.String())
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when object storage is not configured", rec.Code)
	}

	created := store.snapshotCreated()
	if len(created) != 1 {
		t.Fatalf("createLibraryFile called %d times, want 1 -- the owner has to be able to SEE "+
			"the failure in their Library", len(created))
	}
	if strings.Contains(created[0].BlobUrl, "local://") {
		t.Errorf("blobUrl = %q; a local:// placeholder pretends the bytes are somewhere they are not",
			created[0].BlobUrl)
	}

	statuses := store.snapshotStatuses()
	if len(statuses) != 1 {
		t.Fatalf("setLibraryFileStatus called %d times, want 1", len(statuses))
	}
	if statuses[0].Status != libraryStatusFailed {
		t.Errorf("status = %q, want %q", statuses[0].Status, libraryStatusFailed)
	}
	if strings.TrimSpace(statuses[0].FailureReason) == "" {
		t.Error("the failed row carries no failureReason; `failed` with no reason is what the " +
			"concept's own field description forbids")
	}
	if statuses[0].FileId != created[0].FileId {
		t.Errorf("the failure was recorded against %q, not the file just created (%q)",
			statuses[0].FileId, created[0].FileId)
	}
}

// The uploader is present but storage refuses. Same posture, different code:
// the bytes are still not stored, so the row is still failed and the caller
// still is not told 201.
func TestArtifactUploadWithAFailingUploaderRecordsAFailedRow(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	blob.upErr = fmt.Errorf("container is read-only")
	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Store: store,
		PromotionWait: 50 * time.Millisecond,
	})

	rec := postUpload(t, h, "user-a", "notes.md", "text/markdown", []byte("# hi"), nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when storage refuses. body: %s", rec.Code, rec.Body.String())
	}
	statuses := store.snapshotStatuses()
	if len(statuses) != 1 || statuses[0].Status != libraryStatusFailed ||
		strings.TrimSpace(statuses[0].FailureReason) == "" {
		t.Fatalf("want one failed status with a reason, got %+v", statuses)
	}
}

// The promotion is an automation, so the first read can genuinely miss. A
// bounded wait covers it; running out is a warning and an empty artifactId,
// never a failed upload whose bytes are already durable.
func TestArtifactUploadWaitsForThePromotionThenGivesUpGracefully(t *testing.T) {
	t.Run("lands on a later poll", func(t *testing.T) {
		store := newFakeLibraryStore()
		store.promoteAfter = 2
		blob := newFakeBlob()
		h := NewArtifactHandler(ArtifactHandlerOptions{
			Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
			PromotionWait: time.Second, PromotionPoll: time.Millisecond,
		})
		rec := postUpload(t, h, "user-a", "n.md", "text/markdown", []byte("x"), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", rec.Code)
		}
		if decodeUpload(t, rec).ArtifactId == "" {
			t.Error("the artifactId never resolved despite the promotion landing on poll 3")
		}
	})

	t.Run("never lands", func(t *testing.T) {
		store := newFakeLibraryStore()
		store.promoteAfter = 1 << 30
		blob := newFakeBlob()
		h := NewArtifactHandler(ArtifactHandlerOptions{
			Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
			PromotionWait: 10 * time.Millisecond, PromotionPoll: time.Millisecond,
		})
		rec := postUpload(t, h, "user-a", "n.md", "text/markdown", []byte("x"), nil)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 -- the bytes are stored and the row is the caller's", rec.Code)
		}
		resp := decodeUpload(t, rec)
		if resp.FileId == "" {
			t.Error("the fileId must still come back")
		}
		if resp.ArtifactId != "" {
			t.Errorf("artifactId = %q, want empty when the promotion never landed", resp.ArtifactId)
		}
	})
}

// ---------------------------------------------------------------------------
// download / export
// ---------------------------------------------------------------------------

// seedFileArtifact wires one file artifact end to end in the fake store, owned
// by owner.
func seedFileArtifact(store *fakeLibraryStore, blob *fakeBlob, owner, artifactId, fileId string, data []byte, mime, name string) {
	ref := LibraryFileConceptRef(fileId)
	url := "https://example.blob.core.windows.net/lib/library/" + owner + "/" + fileId + "/" + name
	blob.objects[url] = append([]byte(nil), data...)
	store.artifacts[artifactId] = &LibraryArtifactRow{
		ID: artifactId, Kind: "file", SourceConceptRef: ref, Title: name, MimeType: mime,
	}
	store.files[ref] = &LibraryFileRow{
		ID: fileId, Name: name, MimeType: mime, Size: len(data), BlobUrl: url, Status: "ready",
	}
	store.ownerOf[artifactId] = owner
	store.ownerOf[ref] = owner
}

func TestArtifactContentStreamsAFileWithItsHeaders(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	data := []byte("%PDF-1.7\nbytes\n")
	seedFileArtifact(store, blob, "user-a", "artifact-1", "file-1", data, "application/pdf", "report.pdf")

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})
	rec := getContent(t, h, "user-a", "artifact-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", got)
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(data)) {
		t.Errorf("Content-Length = %q, want %d", got, len(data))
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="report.pdf"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if rec.Body.String() != string(data) {
		t.Errorf("body = %q, want the stored bytes", rec.Body.String())
	}
	// NEVER A REDIRECT. There are no signed URLs in this design and the bytes
	// come through the bff after the backing row was admitted for this caller.
	if rec.Code >= 300 && rec.Code < 400 {
		t.Fatal("the export route redirected; it must stream")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("Location = %q; the export route must never redirect to storage", loc)
	}
}

// A SECOND USER'S DOWNLOAD IS 404 (design 6, item 1).
//
// 404 rather than 403, so id probing cannot tell "exists but not mine" from
// "does not exist" -- the attachment precedent, and the only answer consistent
// with reads that resolve under the caller's own actor.
func TestArtifactContentIsNotVisibleToASecondUser(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	data := []byte("secret bytes")
	seedFileArtifact(store, blob, "user-a", "artifact-1", "file-1", data, "text/plain", "secret.txt")

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})

	// The reachable positive: the owner CAN read it, so a 404 for the stranger
	// is a refusal rather than a broken fixture.
	if rec := getContent(t, h, "user-a", "artifact-1"); rec.Code != http.StatusOK {
		t.Fatalf("the owner got %d; the fixture is broken and the negative below proves nothing", rec.Code)
	}

	rec := getContent(t, h, "user-b", "artifact-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a second user got %d, want 404. body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), string(data)) {
		t.Fatal("a second user was served the bytes")
	}
}

// The index row is the caller's but the BACKING row is not -- separately
// admitted, so this must still be 404. (The real store gets this from the
// engine; the fake models it with two owner entries.)
func TestArtifactContentReChecksTheBackingRow(t *testing.T) {
	store := newFakeLibraryStore()
	blob := newFakeBlob()
	seedFileArtifact(store, blob, "user-a", "artifact-1", "file-1", []byte("x"), "text/plain", "x.txt")
	store.ownerOf[LibraryFileConceptRef("file-1")] = "user-b" // the file is somebody else's

	h := NewArtifactHandler(ArtifactHandlerOptions{
		Logger: quietLogger(), Bucket: "lib", Uploader: blob, Downloader: blob, Store: store,
	})
	if rec := getContent(t, h, "user-a", "artifact-1"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the backing row is not admitted", rec.Code)
	}
}

// A NOTE EXPORTS AS MARKDOWN (design 6, item 1, and D9's "one route exports
// the whole Library").
func TestArtifactContentExportsANoteAsMarkdown(t *testing.T) {
	store := newFakeLibraryStore()
	ref := "v1:notes:note:note-1"
	store.artifacts["artifact-note"] = &LibraryArtifactRow{
		ID: "artifact-note", Kind: "note", SourceConceptRef: ref, Title: "Weekly review",
	}
	store.bodies[ref] = &LibraryExportBody{Title: "Weekly review", Body: "# Weekly review\n\n- one\n", Markdown: true}
	store.ownerOf["artifact-note"] = "user-a"
	store.ownerOf[ref] = "user-a"

	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: store})
	rec := getContent(t, h, "user-a", "artifact-note")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/markdown; charset=utf-8", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="Weekly-review.md"` {
		t.Errorf("Content-Disposition = %q, want a filename derived from the title with .md", got)
	}
	if rec.Body.String() != "# Weekly review\n\n- one\n" {
		t.Errorf("body = %q, want the note's own body", rec.Body.String())
	}
	// A stranger gets 404 here too -- the export path is not a way around the
	// tier just because the bytes are text.
	if rec := getContent(t, h, "user-b", "artifact-note"); rec.Code != http.StatusNotFound {
		t.Errorf("a second user got %d for a note export, want 404", rec.Code)
	}
}

// A memory is plain text, not markdown -- its content is a sentence, not a
// document. Pins the other half of the render decision.
func TestArtifactContentExportsAMemoryAsPlainText(t *testing.T) {
	store := newFakeLibraryStore()
	ref := "v1:library:memory:mem-1"
	store.artifacts["artifact-mem"] = &LibraryArtifactRow{
		ID: "artifact-mem", Kind: "memory", SourceConceptRef: ref, Title: "Prefers dark mode",
	}
	store.bodies[ref] = &LibraryExportBody{Title: "Prefers dark mode", Body: "The user prefers dark mode."}
	store.ownerOf["artifact-mem"] = "user-a"
	store.ownerOf[ref] = "user-a"

	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: store})
	rec := getContent(t, h, "user-a", "artifact-mem")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasSuffix(got, `.txt"`) {
		t.Errorf("Content-Disposition = %q, want a .txt filename", got)
	}
}

// A kind with no exportable body is 404, the SAME answer a denied row gets --
// which is the point: the response must not reintroduce the distinction the
// authorization model erased.
func TestArtifactContentIs404ForAKindWithNoBody(t *testing.T) {
	store := newFakeLibraryStore()
	store.artifacts["artifact-todo"] = &LibraryArtifactRow{
		ID: "artifact-todo", Kind: "todo", SourceConceptRef: "v1:todos:todo:t-1", Title: "Buy milk",
	}
	store.ownerOf["artifact-todo"] = "user-a"

	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: store})
	if rec := getContent(t, h, "user-a", "artifact-todo"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestArtifactContentRefusesAnUnauthenticatedCaller(t *testing.T) {
	store := newFakeLibraryStore()
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: store})

	req := httptest.NewRequest(http.MethodGet, "/artifacts/artifact-1/content", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// routing, names and MIME
// ---------------------------------------------------------------------------

func TestArtifactPathParsing(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		for path, want := range map[string]string{
			"/artifacts/artifact-1/content":             "artifact-1",
			"/api/artifacts/artifact-1/content":         "artifact-1",
			"/artifacts/v1:library:artifact:x/content":  "v1:library:artifact:x",
			"/artifacts//content":                       "",
			"/artifacts/artifact-1":                     "",
			"/artifacts":                                "",
			"/artifacts/artifact-1/content/extra":       "",
			"/artifacts/artifact-1/bundles":             "",
			"/spaces/s1/attachments/a1":                 "",
			"/artifacts/artifact-1/content/../../other": "",
		} {
			got, ok := parseArtifactContentPath(path)
			if want == "" {
				if ok {
					t.Errorf("parseArtifactContentPath(%q) = (%q, true), want a refusal", path, got)
				}
				continue
			}
			if !ok || got != want {
				t.Errorf("parseArtifactContentPath(%q) = (%q, %v), want (%q, true)", path, got, ok, want)
			}
		}
	})

	t.Run("collection", func(t *testing.T) {
		for path, want := range map[string]bool{
			"/artifacts":                    true,
			"/artifacts/":                   true,
			"/api/artifacts":                true,
			"/api/artifacts/":               true,
			"/artifacts/artifact-1":         false,
			"/artifacts/artifact-1/content": false,
			"/sites/":                       false,
		} {
			if got := isArtifactCollectionPath(path); got != want {
				t.Errorf("isArtifactCollectionPath(%q) = %v, want %v", path, got, want)
			}
		}
	})
}

// POST to a path that is not the collection is a 404, not an upload. Without
// this, POST /artifacts/{id}/content would fall into the upload branch.
func TestArtifactHandlerRejectsPostToAnItemPath(t *testing.T) {
	store := newFakeLibraryStore()
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: store})

	body, ct := uploadBody(t, "x.md", "text/markdown", []byte("x"), nil)
	req := httptest.NewRequest(http.MethodPost, "/artifacts/artifact-1/content", body)
	req.Header.Set("Content-Type", ct)
	req = req.WithContext(auth.ContextWithUserActor(req.Context(), "user-a"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(store.snapshotCreated()) != 0 {
		t.Error("a POST to an item path created a file")
	}
}

func TestArtifactHandlerRejectsOtherMethods(t *testing.T) {
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: newFakeLibraryStore()})
	req := httptest.NewRequest(http.MethodDelete, "/artifacts/artifact-1/content", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestLibraryFormatForMIME(t *testing.T) {
	for mime, want := range map[string]string{
		"text/markdown":                "markdown",
		"text/markdown; charset=utf-8": "markdown",
		"TEXT/MARKDOWN":                "markdown",
		"application/pdf":              "pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "document",
		"text/csv":                    "spreadsheet",
		"application/vnd.ms-excel":    "spreadsheet",
		"image/png":                   "image",
		"image/svg+xml":               "image",
		"text/plain":                  "text",
		"text/html":                   "text",
		"application/json":            "text",
		"application/octet-stream":    "other",
		"application/x-memql-unknown": "other",
		"":                            "other",
		"application/zip":             "other",
		"video/mp4":                   "other",
		"application/x-tar; foo=bar":  "other",
		"application/vnd.oasis.opendocument.spreadsheet": "spreadsheet",
	} {
		if got := LibraryFormatForMIME(mime); got != want {
			t.Errorf("LibraryFormatForMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestSanitizeLibraryFileName(t *testing.T) {
	for in, want := range map[string]string{
		"notes.md":               "notes.md",
		"  notes.md  ":           "notes.md",
		"../../etc/passwd":       "passwd",
		`C:\Users\me\report.pdf`: "report.pdf",
		"a/b/c/deep.txt":         "deep.txt",
		"..":                     "upload",
		".":                      "upload",
		"":                       "upload",
		"/":                      "upload",
		"say \"hi\".txt":         "say hi.txt",
		"line\nbreak.txt":        "linebreak.txt",
		strings.Repeat("é", 400): strings.Repeat("é", libraryMaxFileNameRunes),
	} {
		if got := sanitizeLibraryFileName(in); got != want {
			t.Errorf("sanitizeLibraryFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLibraryFileConceptRefIsIdempotent(t *testing.T) {
	if got := LibraryFileConceptRef("abc"); got != "v1:library:file:abc" {
		t.Errorf("bare: got %q", got)
	}
	if got := LibraryFileConceptRef("v1:library:file:abc"); got != "v1:library:file:abc" {
		t.Errorf("already canonical: got %q -- it was double-prefixed", got)
	}
	if got := LibraryFileConceptRef("  "); got != "" {
		t.Errorf("blank: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// the real front end
// ---------------------------------------------------------------------------

// recordingExecutor captures every statement EngineLibraryStore renders and
// runs it through a real engine's front end, so a call site that would fail at
// parse or name resolution fails HERE instead of at the first production call.
type recordingExecutor struct {
	t          *testing.T
	engine     *memqlengine.MemQLEngine
	statements []string
}

func (r *recordingExecutor) Execute(_ context.Context, query string) (any, error) {
	r.statements = append(r.statements, query)
	if _, err := r.engine.Parse(query); err != nil {
		return nil, fmt.Errorf("the engine refused %s: %w", query, err)
	}
	// Parse succeeded; return an empty result, which every caller treats as
	// "no such row". The point of this test is the front end, not the store.
	return nil, nil
}

func newLibraryDSLEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (the embedded dsl/ tree): %v", err)
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	eng.Logger = quietLogger()
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	return eng
}

// EVERY STATEMENT EngineLibraryStore BUILDS, THROUGH THE REAL FRONT END.
//
// Not a syntax check -- eng.Parse resolves the construct name too, so a call
// site naming a mutation, query or builtin that no .memql file declares fails
// here. That upgrade is the whole value: syntax alone stays green while every
// call fails at execute on a real cluster, which is the shape memql#4256 and
// #1454 were both filed on.
//
// It has already earned its place. libraryAddArtifactLabel is a BUILTIN, and
// the bare `name(k: v)` form every other call site here uses is refused with
// "requires a JSON object argument". Nothing else in this suite could have
// noticed: the fake store never renders a statement.
func TestLibraryStoreCallSitesResolveThroughTheRealEngine(t *testing.T) {
	eng := newLibraryDSLEngine(t)
	rec := &recordingExecutor{t: t, engine: eng}
	store := NewEngineLibraryStore(rec)
	ctx := context.Background()

	// Awkward strings on purpose: an apostrophe, embedded quotes, a
	// backslash, a newline and a non-ASCII letter all have to survive the
	// rendering intact rather than breaking out of their literal.
	awkward := "O'Brien \"the\" <file> & co \\ line\nbreak é.md"

	calls := []struct {
		name string
		run  func() error
	}{
		{"createLibraryFile", func() error {
			return store.CreateFile(ctx, LibraryFileCreateParams{
				FileId: "file-1", Name: awkward, MimeType: "text/markdown", Size: 1234,
				Sha256: strings.Repeat("ab", 32), BlobUrl: "library/u-1/file-1/" + awkward,
				Source: "uploaded", Format: "markdown", Summary: awkward,
			})
		}},
		{"createLibraryFile with filing + provenance", func() error {
			// The memql#4781 args, awkward where a string can be: the folder
			// id, the machine name and the path all have to survive rendering.
			return store.CreateFile(ctx, LibraryFileCreateParams{
				FileId: "file-2", Name: "q3.pdf", MimeType: "application/pdf", Size: 9,
				Sha256: strings.Repeat("cd", 32), BlobUrl: "library/u-1/file-2/q3.pdf",
				Source: "uploaded", Format: "pdf",
				FolderId:               "fold-1",
				UploadedFromWorkerId:   "wrk-1",
				UploadedFromWorkerName: awkward,
				UploadedFromPath:       `C:\Users\O'Brien\"q3" é\nreport.pdf`,
			})
		}},
		{"myWorkersWithStatus", func() error {
			_, err := store.OwnedWorker(ctx, "wrk-1")
			return err
		}},
		{"setLibraryFileStatus ready", func() error {
			return store.SetFileStatus(ctx, LibraryFileStatusParams{
				FileId: "file-1", Status: "ready", EmbeddingStatus: "none",
			})
		}},
		{"setLibraryFileStatus failed", func() error {
			return store.SetFileStatus(ctx, LibraryFileStatusParams{
				FileId: "file-1", Status: "failed", FailureReason: awkward,
			})
		}},
		{"libraryArtifactBySourceConceptRef", func() error {
			_, err := store.ArtifactForFile(ctx, "file-1")
			return err
		}},
		{"libraryAddArtifactLabel", func() error {
			return store.AddArtifactLabel(ctx, "artifact-1", awkward)
		}},
		{"libraryArtifactById", func() error {
			_, err := store.Artifact(ctx, "artifact-1")
			return err
		}},
		{"libraryFileById", func() error {
			_, err := store.File(ctx, LibraryFileConceptRef("file-1"))
			return err
		}},
		{"noteById", func() error {
			_, err := store.ExportBody(ctx, "note", "v1:notes:note:n-1")
			return err
		}},
		{"generatedOutputById", func() error {
			_, err := store.ExportBody(ctx, "generated_output", "v1:library:generatedOutput:g-1")
			return err
		}},
		{"memoryById", func() error {
			_, err := store.ExportBody(ctx, "memory", "v1:library:memory:m-1")
			return err
		}},
	}

	for _, c := range calls {
		if err := c.run(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	// A loop that rendered nothing would pass every assertion above.
	if len(rec.statements) != len(calls) {
		t.Fatalf("rendered %d statements for %d call sites; the store is not calling the engine",
			len(rec.statements), len(calls))
	}
}

// THE OTHER DIRECTION: every argument name the store passes must be one the
// construct DECLARES.
//
// Resolution does not cover this. eng.Parse binds the name and stops;
// validateFunctionArgs then iterates the DECLARED fields, and rejectUnknownArgs
// is gated behind the MCP boundary -- so an argument the store invents is
// silently DISCARDED, and the failure is not a crash but a row that never
// received a field the call site believes it wrote (memql#3626, memql#4258).
//
// The two arguments most at risk here are exactly the ones that are NOT in
// LibraryFileCreateParams: `status` and `ownerUserId`. createLibraryFile stamps
// both and declares neither.
func TestLibraryStoreArgumentsAreDeclared(t *testing.T) {
	eng := newLibraryDSLEngine(t)

	// Mirrors what each store method sends, maximal (every optional field
	// populated) so an undeclared optional cannot hide behind being omitted.
	sites := map[string][]string{
		"createLibraryFile":                 {"fileId", "name", "mimeType", "size", "sha256", "blobUrl", "source", "format", "summary", "folderId", "uploadedFromWorkerId", "uploadedFromWorkerName", "uploadedFromPath"},
		"setLibraryFileStatus":              {"fileId", "status", "summary", "embeddingStatus", "failureReason"},
		"libraryArtifactBySourceConceptRef": {"sourceConceptRef"},
		"libraryArtifactById":               {"artifactId"},
		"libraryFileById":                   {"fileId"},
		"noteById":                          {"noteId"},
		"generatedOutputById":               {"outputId"},
		"memoryById":                        {"memoryId"},
	}

	var checked int
	for name, passed := range sites {
		fn, err := eng.Functions().Get(name)
		if err != nil || fn == nil {
			t.Errorf("%s: not in the function registry: %v", name, err)
			continue
		}
		declared := map[string]bool{}
		if fn.ArgsSchema != nil {
			for _, f := range fn.ArgsSchema.Fields {
				declared[f.Name] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("%s declares no args block; the store passes %d argument(s)", name, len(passed))
			continue
		}
		checked++
		for _, arg := range passed {
			if !declared[arg] {
				t.Errorf("%s: the store passes %q, which the construct does not declare. It is "+
					"not refused -- the value is silently discarded and the row never receives it "+
					"(memql#3626).", name, arg)
			}
		}
	}
	if checked != len(sites) {
		t.Fatalf("checked %d of %d call sites", checked, len(sites))
	}

	// The negative that proves the check can fail: createLibraryFile must NOT
	// declare `status` or `ownerUserId`. Both are stamped server-side, and a
	// caller naming either is what the concept's @serverSet exists to refuse.
	fn, err := eng.Functions().Get("createLibraryFile")
	if err != nil || fn == nil || fn.ArgsSchema == nil {
		t.Fatalf("createLibraryFile: %v", err)
	}
	for _, f := range fn.ArgsSchema.Fields {
		if f.Name == "status" || f.Name == "ownerUserId" {
			t.Errorf("createLibraryFile declares %q; the store's params type omits it precisely "+
				"because the mutation stamps it", f.Name)
		}
	}
}

// THE MUX REGRESSION THE BARE SPELLING EXISTS FOR.
//
// Registered exactly the way app/transport_artifacts.go registers -- "POST "
// and "GET " on every ArtifactPaths() entry -- POST /artifacts must REACH the
// handler. With only the subtree pattern "/artifacts/" registered, ServeMux
// answers it with a 301 to /artifacts/, and a 301 on a POST loses the body:
// the upload silently becomes a redirect the client either does not follow or
// follows without its bytes.
func TestArtifactRoutesDispatchThroughAServeMux(t *testing.T) {
	h := NewArtifactHandler(ArtifactHandlerOptions{Logger: quietLogger(), Store: newFakeLibraryStore()})
	mux := http.NewServeMux()
	for _, p := range ArtifactPaths() {
		mux.Handle("POST "+p, h)
		mux.Handle("GET "+p, h)
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		// 401 is the handler answering (no actor on these bare requests), which
		// is the whole assertion: the request REACHED it. A 301 or a 404 would
		// mean the mux never got there.
		{http.MethodPost, "/artifacts", http.StatusUnauthorized},
		{http.MethodPost, "/artifacts/", http.StatusUnauthorized},
		{http.MethodGet, "/artifacts/artifact-1/content", http.StatusUnauthorized},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s -> %d, want %d (a 301 here is the subtree-redirect defect the bare "+
				"/artifacts spelling exists to prevent; a 404 means no pattern matched)",
				tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// The paths declaration is the front door's only view of these routes, so its
// shape is pinned here rather than left to the generator's golden file alone.
func TestArtifactPathsCarryBothSpellings(t *testing.T) {
	paths := ArtifactPaths()
	want := map[string]bool{"/artifacts": false, "/artifacts/": false}
	for _, p := range paths {
		if _, ok := want[p]; ok {
			want[p] = true
		}
	}
	for p, found := range want {
		if !found {
			t.Errorf("ArtifactPaths() does not carry %q; got %v", p, paths)
		}
	}

	// AUTHENTICATED, so it is in none of the three unauthenticated aggregates.
	// Membership in any of them would make the routes reachable without a
	// bearer on every verifier-consuming node -- the opposite of what they are.
	for name, list := range map[string][]string{
		"PublicPaths":            PublicPaths(),
		"HandlerAuthorizedPaths": HandlerAuthorizedPaths(),
		"SelfAuthenticatedPaths": SelfAuthenticatedPaths(),
	} {
		for _, p := range list {
			if p == "/artifacts" || p == "/artifacts/" {
				t.Errorf("%s() declares %q; POST /artifacts and GET /artifacts/{id}/content are "+
					"authenticated routes and belong in none of the three aggregates", name, p)
			}
		}
	}
}
