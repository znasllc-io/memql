package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

const exportPerson = "v1:identity:user:3f6a9d2e-71b4-4c0e-8a55-2d9e0b6c4f18"

// fakeExportDownloads stands in for the accounts plug-in. Its states mirror the
// record's: only a complete export that was not removed resolves, which is the
// contract ResolveForDownload documents.
type fakeExportDownloads struct {
	mu        sync.Mutex
	exports   map[string]fakeExport
	recordErr error
	openErr   error
	slow      bool

	resolved []string
	recorded []fakeRecorded
	opened   []fakeOpened
}

type fakeExport struct {
	file    AccountExportFile
	state   string // draft | running | complete
	removed bool
	data    []byte
}

type fakeRecorded struct{ exportID, userID, rangeAsked string }
type fakeOpened struct{ offset, length int64 }

func (f *fakeExportDownloads) ResolveForDownload(_ context.Context, id string) (AccountExportFile, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, id)
	e, ok := f.exports[id]
	if !ok || e.state != "complete" || e.removed {
		return AccountExportFile{}, false, nil
	}
	return e.file, true, nil
}

func (f *fakeExportDownloads) Open(_ context.Context, file AccountExportFile, offset, length int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, fakeOpened{offset, length})
	if f.openErr != nil {
		return nil, f.openErr
	}
	data := f.exports[file.ID].data
	end := int64(len(data))
	if length >= 0 {
		end = offset + length
	}
	part := data[offset:end]
	if f.slow {
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			half := len(part) / 2
			_, _ = writer.Write(part[:half])
			time.Sleep(75 * time.Millisecond)
			_, _ = writer.Write(part[half:])
		}()
		return reader, nil
	}
	return io.NopCloser(bytes.NewReader(part)), nil
}

func (f *fakeExportDownloads) RecordDownload(_ context.Context, file AccountExportFile, userID, rangeAsked string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.recorded = append(f.recorded, fakeRecorded{file.ID, userID, rangeAsked})
	return nil
}

var exportBytes = []byte("MEMQLEXPORT-0123456789-abcdefghij")

func newFakeExports() *fakeExportDownloads {
	file := func(id string) AccountExportFile {
		return AccountExportFile{
			ID: id, AccountID: "acct-1", FileName: "primary account 2026-11-12.memqlx",
			ObjectKey: "exports/" + id, SHA256: "abc", SizeBytes: int64(len(exportBytes)),
		}
	}
	return &fakeExportDownloads{exports: map[string]fakeExport{
		"exp-done":    {file: file("exp-done"), state: "complete", data: exportBytes},
		"exp-draft":   {file: file("exp-draft"), state: "draft", data: exportBytes},
		"exp-removed": {file: file("exp-removed"), state: "complete", removed: true, data: exportBytes},
	}}
}

func newTestExportHandler(d AccountExportDownloads) *AccountExportHandler {
	return NewAccountExportHandler(AccountExportHandlerOptions{
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Downloads: d,
	})
}

// personContext is a person's own signed-in session, as the server's
// authentication leaves it in the request context.
func personContext(role auth.Role, extraClaims map[string]any) context.Context {
	claims := map[string]any{"sub": exportPerson, "role": string(role)}
	for k, v := range extraClaims {
		claims[k] = v
	}
	ctx := auth.ContextWithClaims(context.Background(), claims)
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: exportPerson, Role: role})
}

func exportRequest(ctx context.Context, method, path, rangeHeader string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	if rangeHeader != "" {
		r.Header.Set("Range", rangeHeader)
	}
	return r
}

func serveExport(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// Every refusal is the same 404 with the same body as an unknown id, and none
// of them opens or audits anything.
func TestAccountExportRefusalsAreAllTheSame404(t *testing.T) {
	fake := newFakeExports()
	h := newTestExportHandler(fake)
	owner := personContext(auth.RoleOwner, nil)

	unknown := serveExport(h, exportRequest(owner, http.MethodGet, "/accounts/exports/exp-nope/content", ""))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown id answered %d, want 404", unknown.Code)
	}
	wantBody := unknown.Body.String()

	cases := map[string]*http.Request{
		"no credential":   exportRequest(nil, http.MethodGet, "/accounts/exports/exp-done/content", ""),
		"anonymous":       exportRequest(auth.ContextWithAnonymousActor(context.Background()), http.MethodGet, "/accounts/exports/exp-done/content", ""),
		"without part":    exportRequest(personContext(auth.RoleAdmin, nil), http.MethodGet, "/accounts/exports/exp-done/content", ""),
		"access token":    exportRequest(personContext(auth.RoleOwner, map[string]any{"pat_id": "pat-1"}), http.MethodGet, "/accounts/exports/exp-done/content", ""),
		"service account": exportRequest(personContext(auth.RoleOwner, map[string]any{"class": auth.ForwardedClassServiceAccount}), http.MethodGet, "/accounts/exports/exp-done/content", ""),
		"app session":     exportRequest(personContext(auth.RoleOwner, map[string]any{"class": auth.ForwardedClassAppSession}), http.MethodGet, "/accounts/exports/exp-done/content", ""),
		"borrowed":        exportRequest(auth.ContextWithUserActor(context.Background(), exportPerson), http.MethodGet, "/accounts/exports/exp-done/content", ""),
		"draft":           exportRequest(owner, http.MethodGet, "/accounts/exports/exp-draft/content", ""),
		"removed":         exportRequest(owner, http.MethodGet, "/accounts/exports/exp-removed/content", ""),
	}
	for name, r := range cases {
		rec := serveExport(h, r)
		if rec.Code != http.StatusNotFound || rec.Body.String() != wantBody {
			t.Errorf("%s: answered %d %q, want 404 %q", name, rec.Code, rec.Body.String(), wantBody)
		}
	}
	if len(fake.recorded) != 0 || len(fake.opened) != 0 {
		t.Fatalf("a refusal audited or opened something: recorded=%v opened=%v", fake.recorded, fake.opened)
	}
}

// An id that is empty, carries a slash once decoded, or is a dot-dot is refused
// before the plug-in is asked about it.
func TestAccountExportMalformedIdsNeverReachThePlugin(t *testing.T) {
	fake := newFakeExports()
	h := newTestExportHandler(fake)
	owner := personContext(auth.RoleOwner, nil)
	for _, path := range []string{
		"/accounts/exports//content",
		"/accounts/exports/../content",
		"/accounts/exports/a..b/content",
		"/accounts/exports/a%2Fb/content",
		"/accounts/exports/exp-done/content/more",
		"/accounts/exports/exp-done",
		"/accounts/exports/exp-done/other",
	} {
		rec := serveExport(h, exportRequest(owner, http.MethodGet, path, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, rec.Code)
		}
	}
	if len(fake.resolved) != 0 {
		t.Fatalf("a malformed id reached the plug-in: %v", fake.resolved)
	}
}

func TestAccountExportServesTheWholeFile(t *testing.T) {
	fake := newFakeExports()
	h := newTestExportHandler(fake)
	rec := serveExport(h, exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/exp-done/content", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d %q, want 200", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), exportBytes) {
		t.Fatalf("body %q, want %q", rec.Body.Bytes(), exportBytes)
	}
	want := map[string]string{
		"Content-Type":           "application/octet-stream",
		"Content-Disposition":    `attachment; filename="primary account 2026-11-12.memqlx"`,
		"Content-Length":         "33",
		"Accept-Ranges":          "bytes",
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(fake.recorded) != 1 || fake.recorded[0] != (fakeRecorded{"exp-done", exportPerson, ""}) {
		t.Fatalf("audit events %v, want one for the whole file", fake.recorded)
	}
	if len(fake.opened) != 1 || fake.opened[0] != (fakeOpened{0, -1}) {
		t.Fatalf("opened %v, want the whole object", fake.opened)
	}
}

// A developer holds the part as an owner does.
func TestAccountExportAdmitsADeveloper(t *testing.T) {
	h := newTestExportHandler(newFakeExports())
	rec := serveExport(h, exportRequest(personContext(auth.RoleDeveloper, nil), http.MethodGet, "/accounts/exports/exp-done/content", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("a developer answered %d, want 200", rec.Code)
	}
}

func TestAccountExportHonoursOneRange(t *testing.T) {
	fake := newFakeExports()
	h := newTestExportHandler(fake)
	rec := serveExport(h, exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/exp-done/content", "bytes=5-10"))
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("answered %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != string(exportBytes[5:11]) {
		t.Fatalf("body %q, want %q", got, exportBytes[5:11])
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 5-10/33" {
		t.Errorf("Content-Range = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "6" {
		t.Errorf("Content-Length = %q", got)
	}
	if len(fake.recorded) != 1 || fake.recorded[0].rangeAsked != "bytes=5-10" {
		t.Fatalf("audit events %v, want one naming the range", fake.recorded)
	}
	if fake.opened[0] != (fakeOpened{5, 6}) {
		t.Fatalf("opened %v, want offset 5 length 6", fake.opened)
	}
}

func TestAccountExportRangePastTheEndIs416(t *testing.T) {
	fake := newFakeExports()
	h := newTestExportHandler(fake)
	rec := serveExport(h, exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/exp-done/content", "bytes=100-"))
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("answered %d, want 416", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */33" {
		t.Errorf("Content-Range = %q", got)
	}
	if len(fake.opened) != 0 {
		t.Fatalf("an unsatisfiable range opened the object: %v", fake.opened)
	}
	if len(fake.recorded) != 1 || fake.recorded[0].rangeAsked != "bytes=100-" {
		t.Fatalf("audit events %v, want one naming the range", fake.recorded)
	}
}

// Several ranges are answered whole: Range is advisory, and the single-range
// subset is all this route serves.
func TestAccountExportMultiRangeIsAnsweredWhole(t *testing.T) {
	h := newTestExportHandler(newFakeExports())
	rec := serveExport(h, exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/exp-done/content", "bytes=0-1,5-6"))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), exportBytes) {
		t.Fatalf("answered %d %q, want 200 with the whole file", rec.Code, rec.Body.Bytes())
	}
}

// No download without its audit event.
func TestAccountExportAuditFailureSendsNoByte(t *testing.T) {
	fake := newFakeExports()
	fake.recordErr = errors.New("audit write refused")
	h := newTestExportHandler(fake)
	rec := serveExport(h, exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/exp-done/content", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("answered %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), string(exportBytes[:10])) || len(fake.opened) != 0 {
		t.Fatalf("bytes were read or sent without an audit event: opened=%v", fake.opened)
	}
}

func TestAccountExportOffersOnlyGet(t *testing.T) {
	fake := newFakeExports()
	h := newTestExportHandler(fake)
	for _, m := range []string{http.MethodHead, http.MethodPost, http.MethodDelete} {
		rec := serveExport(h, exportRequest(personContext(auth.RoleOwner, nil), m, "/accounts/exports/exp-done/content", ""))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s answered %d, want 405", m, rec.Code)
		}
	}
	if len(fake.resolved) != 0 {
		t.Fatalf("a refused method reached the plug-in: %v", fake.resolved)
	}
}

// A large file outlives the server's ordinary write deadline while bytes flow.
func TestAccountExportSurvivesServerWriteDeadline(t *testing.T) {
	fake := newFakeExports()
	fake.slow = true
	h := newTestExportHandler(fake)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(personContext(auth.RoleOwner, nil)))
	}))
	s.Config.WriteTimeout = 20 * time.Millisecond
	s.Start()
	defer s.Close()
	response, err := s.Client().Get(s.URL + "/accounts/exports/exp-done/content")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || !bytes.Equal(body, exportBytes) {
		t.Fatalf("download ended early: bytes=%d error=%v", len(body), err)
	}
}

func TestAccountExportPathsCarryTheRoute(t *testing.T) {
	paths := AccountExportPaths()
	if len(paths) == 0 || paths[0] != "/accounts/exports/" {
		t.Fatalf("AccountExportPaths() = %v, want /accounts/exports/ first", paths)
	}
}
