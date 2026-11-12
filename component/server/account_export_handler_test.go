package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/httpstream"
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
	short     bool // the stored object ends before the size the record names
	extra     bool // Open hands back more than was asked

	openDeadline time.Duration

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

func (f *fakeExportDownloads) Open(ctx context.Context, file AccountExportFile, offset, length int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, fakeOpened{offset, length})
	if d, ok := ctx.Deadline(); ok {
		f.openDeadline = time.Until(d)
	}
	if f.openErr != nil {
		return nil, f.openErr
	}
	data := f.exports[file.ID].data
	end := int64(len(data))
	if length >= 0 {
		end = offset + length
	}
	part := data[offset:end]
	if f.short {
		part = part[:len(part)/2]
	}
	if f.extra {
		part = append(append([]byte{}, part...), []byte("-TRAILING-BYTES")...)
	}
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

const exportSHA = "9f2c1e7a5b3d40c8a6e1f0b2d4c6e8a0b1c3d5e7f9a2b4c6d8e0f1a3b5c7d9e1"

const exportETag = `"sha256-` + exportSHA + `"`

func newFakeExports() *fakeExportDownloads {
	file := func(id string) AccountExportFile {
		return AccountExportFile{
			ID: id, AccountID: "acct-1", FileName: "primary account 2026-11-12.memqlx",
			ObjectKey: "exports/" + id, SHA256: exportSHA, SizeBytes: int64(len(exportBytes)),
		}
	}
	return &fakeExportDownloads{exports: map[string]fakeExport{
		"exp-done":    {file: file("exp-done"), state: "complete", data: exportBytes},
		"exp-draft":   {file: file("exp-draft"), state: "draft", data: exportBytes},
		"exp-removed": {file: file("exp-removed"), state: "complete", removed: true, data: exportBytes},
		"exp-empty": {file: AccountExportFile{ID: "exp-empty", AccountID: "acct-1", FileName: "empty.memqlx",
			ObjectKey: "exports/exp-empty", SHA256: exportSHA, SizeBytes: 0}, state: "complete", data: []byte{}},
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
		"ETag":                   exportETag,
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
	if got := rec.Header().Get("Content-Length"); got == "33" {
		t.Errorf("a 416 announced the file's length")
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
	t.Setenv("MEMQL_SERVER_PUBLIC_PATH", "")
	if paths := AccountExportPaths(); len(paths) != 1 || paths[0] != "/accounts/exports/" {
		t.Fatalf("AccountExportPaths() = %v, want exactly [/accounts/exports/]", paths)
	}
}

// The id is 1 to 128 bytes of letters, digits, "-", "_", "." and ":", taken
// exactly as decoded; anything else is the same 404 before the plug-in is asked.
func TestAccountExportIdAlphabetIsClosed(t *testing.T) {
	fake := newFakeExports()
	h := newTestExportHandler(fake)
	owner := personContext(auth.RoleOwner, nil)
	for name, path := range map[string]string{
		"space":             "/accounts/exports/exp%20done/content",
		"leading space":     "/accounts/exports/%20exp-done/content",
		"control character": "/accounts/exports/exp%07done/content",
		"nul":               "/accounts/exports/exp%00done/content",
		"newline":           "/accounts/exports/exp%0Adone/content",
		"129 bytes":         "/accounts/exports/" + strings.Repeat("a", 129) + "/content",
		"non-ascii":         "/accounts/exports/exp%C3%A9/content",
		"quote":             "/accounts/exports/exp%22/content",
	} {
		rec := serveExport(h, exportRequest(owner, http.MethodGet, path, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: answered %d, want 404", name, rec.Code)
		}
	}
	if len(fake.resolved) != 0 {
		t.Fatalf("an id outside the alphabet reached the plug-in: %q", fake.resolved)
	}
	// The whole alphabet, at the 128-byte bound, does reach it.
	ok := "v1:accounts:export:" + strings.Repeat("aZ9-_.", 30)[:128-len("v1:accounts:export:")]
	serveExport(h, exportRequest(owner, http.MethodGet, "/accounts/exports/"+ok+"/content", ""))
	if len(fake.resolved) != 1 || fake.resolved[0] != ok {
		t.Fatalf("a 128-byte id in the alphabet was not asked about: %q", fake.resolved)
	}
}

func rangeRequest(rangeHeader, ifRange string) *http.Request {
	r := exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/exp-done/content", rangeHeader)
	if ifRange != "" {
		r.Header.Set("If-Range", ifRange)
	}
	return r
}

// The single-range cases, each with the bytes, Content-Range, Content-Length
// and the length Open was asked for agreeing.
func TestAccountExportRangeCases(t *testing.T) {
	size := len(exportBytes)
	cases := []struct {
		name, header string
		start, end   int
	}{
		{"first byte", "bytes=0-0", 0, 0},
		{"open end", "bytes=5-", 5, size - 1},
		{"suffix", "bytes=-5", size - 5, size - 1},
		{"suffix past the start", "bytes=-100", 0, size - 1},
		{"end past the end is clamped", "bytes=30-100", 30, size - 1},
	}
	for _, tc := range cases {
		fake := newFakeExports()
		rec := serveExport(newTestExportHandler(fake), rangeRequest(tc.header, ""))
		if rec.Code != http.StatusPartialContent {
			t.Errorf("%s: answered %d, want 206", tc.name, rec.Code)
			continue
		}
		want := exportBytes[tc.start : tc.end+1]
		if !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("%s: body %q, want %q", tc.name, rec.Body.Bytes(), want)
		}
		if got, w := rec.Header().Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end, size); got != w {
			t.Errorf("%s: Content-Range %q, want %q", tc.name, got, w)
		}
		if got, w := rec.Header().Get("Content-Length"), strconv.Itoa(len(want)); got != w {
			t.Errorf("%s: Content-Length %q, want %q", tc.name, got, w)
		}
		if fake.opened[0] != (fakeOpened{int64(tc.start), int64(len(want))}) {
			t.Errorf("%s: opened %v", tc.name, fake.opened)
		}
		if got := rec.Header().Get("ETag"); got != exportETag {
			t.Errorf("%s: ETag %q on a 206", tc.name, got)
		}
	}
}

// A zero suffix is unsatisfiable, and so is any range of an empty file.
func TestAccountExportUnsatisfiableRanges(t *testing.T) {
	for _, tc := range []struct{ id, header, contentRange string }{
		{"exp-done", "bytes=-0", "bytes */33"},
		{"exp-done", "bytes=33-", "bytes */33"},
		{"exp-empty", "bytes=0-", "bytes */0"},
		{"exp-empty", "bytes=-5", "bytes */0"},
	} {
		fake := newFakeExports()
		r := exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/"+tc.id+"/content", tc.header)
		rec := serveExport(newTestExportHandler(fake), r)
		if rec.Code != http.StatusRequestedRangeNotSatisfiable || rec.Header().Get("Content-Range") != tc.contentRange {
			t.Errorf("%s %s: answered %d %q, want 416 %q", tc.id, tc.header, rec.Code, rec.Header().Get("Content-Range"), tc.contentRange)
		}
		if len(fake.opened) != 0 {
			t.Errorf("%s %s: opened the object", tc.id, tc.header)
		}
	}
}

// Malformed or foreign-unit ranges are advisory and answered whole.
func TestAccountExportUnreadableRangesAreAnsweredWhole(t *testing.T) {
	for _, header := range []string{"bytes=5-2", "items=0-5", "bytes=abc", "bytes=-"} {
		rec := serveExport(newTestExportHandler(newFakeExports()), rangeRequest(header, ""))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), exportBytes) {
			t.Errorf("%q: answered %d, want 200 whole", header, rec.Code)
		}
	}
}

func TestAccountExportEmptyFileIsServedWhole(t *testing.T) {
	fake := newFakeExports()
	r := exportRequest(personContext(auth.RoleOwner, nil), http.MethodGet, "/accounts/exports/exp-empty/content", "")
	rec := serveExport(newTestExportHandler(fake), r)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
		t.Fatalf("answered %d, %d bytes, Content-Length %q; want 200, 0, \"0\"", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	if fake.opened[0] != (fakeOpened{0, -1}) {
		t.Fatalf("opened %v", fake.opened)
	}
}

// If-Range: only the exact strong ETag lets the Range stand; anything else is
// the whole file with 200.
func TestAccountExportIfRange(t *testing.T) {
	rec := serveExport(newTestExportHandler(newFakeExports()), rangeRequest("bytes=5-10", exportETag))
	if rec.Code != http.StatusPartialContent || rec.Body.String() != string(exportBytes[5:11]) {
		t.Fatalf("a matching If-Range answered %d %q, want 206", rec.Code, rec.Body.String())
	}
	for name, ifRange := range map[string]string{
		"another etag":   `"sha256-0000"`,
		"weak validator": "W/" + exportETag,
		"a date":         "Wed, 12 Nov 2026 10:00:00 GMT",
		"unquoted":       "sha256-" + exportSHA,
	} {
		rec := serveExport(newTestExportHandler(newFakeExports()), rangeRequest("bytes=5-10", ifRange))
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), exportBytes) {
			t.Errorf("%s: answered %d, want 200 whole", name, rec.Code)
		}
		if rec.Header().Get("ETag") != exportETag {
			t.Errorf("%s: ETag %q", name, rec.Header().Get("ETag"))
		}
	}
	// A mismatched If-Range on an unsatisfiable range is the whole file too:
	// the range is ignored before it is judged.
	rec = serveExport(newTestExportHandler(newFakeExports()), rangeRequest("bytes=100-", `"sha256-0000"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("a mismatched If-Range on a range past the end answered %d, want 200", rec.Code)
	}
}

// If-None-Match and If-Match are not offered: they change nothing.
func TestAccountExportIgnoresOtherPreconditions(t *testing.T) {
	for _, header := range []string{"If-None-Match", "If-Match"} {
		r := rangeRequest("", "")
		r.Header.Set(header, exportETag)
		rec := serveExport(newTestExportHandler(newFakeExports()), r)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), exportBytes) {
			t.Errorf("%s: answered %d, want 200 whole", header, rec.Code)
		}
	}
}

// No ETag without a usable digest: a resume cannot be validated against one.
func TestAccountExportNoDigestNoETag(t *testing.T) {
	for _, sha := range []string{"", "not-hex", `abc"def`} {
		fake := newFakeExports()
		e := fake.exports["exp-done"]
		e.file.SHA256 = sha
		fake.exports["exp-done"] = e
		rec := serveExport(newTestExportHandler(fake), rangeRequest("bytes=5-10", `"sha256-`+sha+`"`))
		if rec.Header().Get("ETag") != "" {
			t.Errorf("digest %q: sent ETag %q", sha, rec.Header().Get("ETag"))
		}
		if rec.Code != http.StatusOK {
			t.Errorf("digest %q: If-Range matched with no ETag, answered %d", sha, rec.Code)
		}
	}
}

// The response's own Content-Length is the contract: never more bytes than it
// announced.
func TestAccountExportNeverSendsMoreThanAnnounced(t *testing.T) {
	fake := newFakeExports()
	fake.extra = true
	rec := serveExport(newTestExportHandler(fake), rangeRequest("", ""))
	if !bytes.Equal(rec.Body.Bytes(), exportBytes) {
		t.Fatalf("sent %q, want exactly the announced %d bytes", rec.Body.Bytes(), len(exportBytes))
	}
	fake = newFakeExports()
	fake.extra = true
	rec = serveExport(newTestExportHandler(fake), rangeRequest("bytes=5-10", ""))
	if rec.Body.String() != string(exportBytes[5:11]) {
		t.Fatalf("range sent %q, want %q", rec.Body.String(), exportBytes[5:11])
	}
}

// A stored object that ends early is a failed transfer: the connection is
// closed, so the client sees an error rather than a short success.
func TestAccountExportShortObjectFailsTheTransfer(t *testing.T) {
	fake := newFakeExports()
	fake.short = true
	h := newTestExportHandler(fake)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(personContext(auth.RoleOwner, nil)))
	}))
	defer s.Close()
	// The failure shows at whichever point the closed connection reaches the
	// client: before the headers (a body this small is still buffered) or in
	// the body. Either is a failed transfer; a clean read is the defect.
	response, err := s.Client().Get(s.URL + "/accounts/exports/exp-done/content")
	if err != nil {
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err == nil {
		t.Fatalf("a short object read as a complete download of %d bytes", len(body))
	}
}

// The bff hop uses the long ceiling, not the Library's 30 minutes.
func TestAccountExportUsesTheLongCeiling(t *testing.T) {
	fake := newFakeExports()
	serveExport(newTestExportHandler(fake), rangeRequest("", ""))
	if fake.openDeadline <= 30*time.Minute || fake.openDeadline > httpstream.LongDownloadLimit {
		t.Fatalf("the stored object was opened under a %v ceiling, want %v", fake.openDeadline, httpstream.LongDownloadLimit)
	}
}
