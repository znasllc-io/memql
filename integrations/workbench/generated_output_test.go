package workbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestDeriveGeneratedOutputId_Deterministic(t *testing.T) {
	a := deriveGeneratedOutputId("workbench_generated", "user-1", "plan-1:/out.txt")
	b := deriveGeneratedOutputId("workbench_generated", "user-1", "plan-1:/out.txt")
	if a != b {
		t.Fatalf("same inputs must yield same id: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "genout-") || len(a) != len("genout-")+16 {
		t.Fatalf("malformed id %q", a)
	}
}

func TestDeriveGeneratedOutputId_Distinct(t *testing.T) {
	base := deriveGeneratedOutputId("workbench_generated", "user-1", "plan-1:/out.txt")
	for name, got := range map[string]string{
		"source": deriveGeneratedOutputId("computer_use", "user-1", "plan-1:/out.txt"),
		"owner":  deriveGeneratedOutputId("workbench_generated", "user-2", "plan-1:/out.txt"),
		"key":    deriveGeneratedOutputId("workbench_generated", "user-1", "plan-1:/other.txt"),
	} {
		if got == base {
			t.Errorf("%s: expected distinct id, got duplicate %q", name, got)
		}
	}
}

func TestFormatForExtension(t *testing.T) {
	cases := map[string]string{
		"notes.md":       "markdown",
		"a/b/report.MD":  "markdown",
		"slides.pdf":     "pdf",
		"memo.docx":      "document",
		"data.csv":       "spreadsheet",
		"sheet.xlsx":     "spreadsheet",
		"diagram.png":    "image",
		"photo.JPEG":     "image",
		"out.txt":        "text",
		"server.go":      "text",
		"config.yaml":    "text",
		"noextension":    "text",
		"archive.tar.gz": "other",
		"blob.bin":       "other",
	}
	for in, want := range cases {
		if got := formatForExtension(in); got != want {
			t.Errorf("formatForExtension(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathBasename(t *testing.T) {
	cases := map[string]string{
		"/ws/report.md": "report.md",
		"report.md":     "report.md",
		"a/b/c.txt":     "c.txt",
		"/ws/dir/":      "dir",
		"":              "",
	}
	for in, want := range cases {
		if got := pathBasename(in); got != want {
			t.Errorf("pathBasename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOutputPayloadRows(t *testing.T) {
	if rows := outputPayloadRows(nil); rows != nil {
		t.Errorf("nil payload should yield nil rows, got %v", rows)
	}
	single := outputPayloadRows(map[string]any{"createdBy": "u"})
	if len(single) != 1 || stringFromRow(single[0], "createdBy") != "u" {
		t.Errorf("bare-map payload mis-normalised: %v", single)
	}
	slice := outputPayloadRows([]any{map[string]any{"createdBy": "u"}, "junk"})
	if len(slice) != 1 {
		t.Errorf("[]any payload should drop non-map elements, got %v", slice)
	}
}

// captureEngine implements IntegrationEngineAccess but only Execute is
// exercised by the promotion path; the rest of the interface is left as a
// nil embed (calling any other method would panic, which is the test we
// want -- the promotion path must touch nothing else).
type captureEngine struct {
	memql.IntegrationEngineAccess
	queries []string
}

func (c *captureEngine) Execute(_ context.Context, q string) (*memql.ExecuteResult, error) {
	c.queries = append(c.queries, q)
	// Return an empty (output-less) result: resolvePlanOwner finds no
	// owner and the promotion short-circuits before the insert. That's
	// fine -- we're asserting which queries the fs_write path issues, not
	// the insert itself (which requires a live engine).
	return &memql.ExecuteResult{}, nil
}

func TestPromoteWorkbenchOutput_NilEngineNoPanic(t *testing.T) {
	i := &Integration{} // engine nil
	i.promoteWorkbenchOutput(context.Background(), "plan-1", "agent-1",
		map[string]any{"path": "/ws/out.txt"}, true)
}

func TestPromoteWorkbenchOutput_ResolvesPlanOwner(t *testing.T) {
	ce := &captureEngine{}
	i := &Integration{}
	i.SetEngine(ce)

	// local=true but with no uploader configured: the upload path is
	// skipped and the promotion still resolves the plan owner as before.
	i.promoteWorkbenchOutput(context.Background(), "plan-1", "agent-1",
		map[string]any{"path": "/ws/out.txt"}, true)

	if len(ce.queries) != 1 {
		t.Fatalf("expected exactly one planById call, got %d: %v", len(ce.queries), ce.queries)
	}
	if !strings.HasPrefix(ce.queries[0], "planById(") {
		t.Errorf("expected planById, got %q", ce.queries[0])
	}
	if !strings.Contains(ce.queries[0], "plan-1") {
		t.Errorf("planById missing planId: %q", ce.queries[0])
	}
}

func TestPromoteWorkbenchOutput_SkipsWhenNoPath(t *testing.T) {
	ce := &captureEngine{}
	i := &Integration{}
	i.SetEngine(ce)

	i.promoteWorkbenchOutput(context.Background(), "plan-1", "agent-1", map[string]any{}, true)
	if len(ce.queries) != 0 {
		t.Errorf("missing path should skip all engine calls, got %v", ce.queries)
	}
}

// fakeUploader records Upload calls and returns a canned URL/error.
type fakeUploader struct {
	calls     int
	lastObj   string
	lastBytes int
	url       string
	err       error
}

func (f *fakeUploader) Upload(_ context.Context, _ /*bucket*/, objectName string, data []byte, _ /*contentType*/ string) (string, error) {
	f.calls++
	f.lastObj = objectName
	f.lastBytes = len(data)
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}

// newTestIntegration wires an Integration with a temp-dir workspace
// Manager, a capture engine, and the given uploader, plus a provisioned
// workspace containing fileName=content. Returns the integration + engine.
func newTestIntegration(t *testing.T, up attachmentUploader, fileName, content string) (*Integration, *captureEngine) {
	t.Helper()
	t.Setenv("MEMQL_WORKBENCH_ROOT", t.TempDir())
	mgr := NewManager()
	ws, err := mgr.provisionForPlan("plan-1")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws.rootPath, fileName), []byte(content), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	ce := &captureEngine{}
	i := &Integration{manager: mgr, engine: ce}
	i.SetAttachmentUploader(up, "test-bucket")
	return i, ce
}

func TestDetectMimeType(t *testing.T) {
	if got := detectMimeType("a.pdf", []byte("%PDF-1.4")); !strings.HasPrefix(got, "application/pdf") {
		t.Errorf("pdf extension: got %q", got)
	}
	// Unknown extension falls back to content sniffing (PNG magic).
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	if got := detectMimeType("blob.unknownext", png); !strings.HasPrefix(got, "image/png") {
		t.Errorf("sniff png: got %q", got)
	}
	if got := detectMimeType("", nil); got != "application/octet-stream" {
		t.Errorf("empty fallback: got %q", got)
	}
}

func TestReadWorkspaceFile(t *testing.T) {
	i, _ := newTestIntegration(t, &fakeUploader{}, "out.txt", "hello bytes")
	data, err := i.readWorkspaceFile("plan-1", "out.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello bytes" {
		t.Errorf("content mismatch: %q", data)
	}
	// Path traversal is rejected by safeJoin.
	if _, err := i.readWorkspaceFile("plan-1", "../escape.txt"); err == nil {
		t.Error("expected traversal to be rejected")
	}
}

func TestUploadAttachmentBytes_HappyPath(t *testing.T) {
	up := &fakeUploader{url: "gs://test-bucket/obj"}
	i, ce := newTestIntegration(t, up, "report.pdf", "%PDF-1.4 body")

	data := []byte("%PDF-1.4 body")
	attID, mime := i.uploadAttachmentBytes(context.Background(), "plan-1", "report.pdf", "report.pdf", "space-1", "user-1", data)
	if attID == "" {
		t.Fatal("expected a non-empty attachmentId")
	}
	if !strings.HasPrefix(attID, "space-1:") {
		t.Errorf("attachmentId should be space-scoped, got %q", attID)
	}
	if !strings.HasPrefix(mime, "application/pdf") {
		t.Errorf("mimeType: got %q", mime)
	}
	if up.calls != 1 || up.lastBytes != len(data) {
		t.Errorf("uploader not invoked with the bytes: calls=%d bytes=%d", up.calls, up.lastBytes)
	}
	// A mutationCreateAttachment must have been issued carrying the id + URL.
	if len(ce.queries) != 1 || !strings.HasPrefix(ce.queries[0], "mutationCreateAttachment(") {
		t.Fatalf("expected mutationCreateAttachment, got %v", ce.queries)
	}
	if !strings.Contains(ce.queries[0], attID) || !strings.Contains(ce.queries[0], "gs://test-bucket/obj") {
		t.Errorf("attachment mutation missing id/url: %q", ce.queries[0])
	}

	// Idempotent: the same (planId, path) yields the same deterministic id.
	up2 := &fakeUploader{url: "gs://test-bucket/obj"}
	i2, _ := newTestIntegration(t, up2, "report.pdf", "%PDF-1.4 body")
	attID2, _ := i2.uploadAttachmentBytes(context.Background(), "plan-1", "report.pdf", "report.pdf", "space-1", "user-1", data)
	if attID2 != attID {
		t.Errorf("non-deterministic attachmentId: %q != %q", attID2, attID)
	}
}

func TestUploadAttachmentBytes_UploaderError(t *testing.T) {
	up := &fakeUploader{err: errors.New("gcs down")}
	i, ce := newTestIntegration(t, up, "out.txt", "data")

	attID, mime := i.uploadAttachmentBytes(context.Background(), "plan-1", "out.txt", "out.txt", "space-1", "user-1", []byte("data"))
	if attID != "" || mime != "" {
		t.Errorf("upload failure must yield empty id/mime, got %q/%q", attID, mime)
	}
	// No attachment row created when the byte upload failed.
	if len(ce.queries) != 0 {
		t.Errorf("expected no mutation on upload failure, got %v", ce.queries)
	}
}

func TestUploadAttachmentBytes_EmptySkips(t *testing.T) {
	up := &fakeUploader{url: "gs://x/y"}
	i, _ := newTestIntegration(t, up, "out.txt", "data")
	if attID, _ := i.uploadAttachmentBytes(context.Background(), "plan-1", "out.txt", "out.txt", "space-1", "user-1", nil); attID != "" {
		t.Errorf("empty data must skip upload, got id %q", attID)
	}
	if up.calls != 0 {
		t.Errorf("uploader must not be called for empty data, calls=%d", up.calls)
	}
}

// TestWorkbenchOutputBytes covers the #742 byte-source selector: local
// reads off disk; cluster reuses the forwarded `content` (no disk, no
// cross-node transfer).
func TestWorkbenchOutputBytes(t *testing.T) {
	i, _ := newTestIntegration(t, &fakeUploader{}, "out.txt", "disk bytes")

	// Local + existing file -> disk bytes.
	if got := i.workbenchOutputBytes("plan-1", "out.txt", map[string]any{}, true); string(got) != "disk bytes" {
		t.Errorf("local existing: got %q", got)
	}
	// Local + missing file -> nil (caller records a pointer row).
	if got := i.workbenchOutputBytes("plan-1", "ghost.txt", map[string]any{}, true); got != nil {
		t.Errorf("local missing: expected nil, got %q", got)
	}
	// Cluster -> the forwarded content IS the bytes; no disk access.
	clusterArgs := map[string]any{"path": "remote.txt", "content": "forwarded bytes"}
	if got := i.workbenchOutputBytes("plan-1", "remote.txt", clusterArgs, false); string(got) != "forwarded bytes" {
		t.Errorf("cluster content: got %q", got)
	}
	// Cluster + no content -> nil.
	if got := i.workbenchOutputBytes("plan-1", "remote.txt", map[string]any{}, false); got != nil {
		t.Errorf("cluster no-content: expected nil, got %q", got)
	}
}

// TestDeriveInlineSummary covers the #1392 producer-side fix: inline
// text deliverables carry a real one-line summary derived from their
// content (heading markers stripped, whitespace collapsed, truncated),
// and unusable bodies yield "" so the field is omitted entirely.
func TestDeriveInlineSummary(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"markdown heading", "# 10 Best German Folklore Tales\n\nOnce upon a time...", "10 Best German Folklore Tales"},
		{"plain first line", "\n\nA curated list of tales.\nMore text.", "A curated list of tales."},
		{"collapses whitespace", "First   line\twith   runs", "First line with runs"},
		{"empty body", "   \n\n  ", ""},
		{"hashes only", "###", ""},
	}
	for _, tc := range cases {
		if got := deriveInlineSummary(tc.body); got != tc.want {
			t.Errorf("%s: deriveInlineSummary(%q) = %q, want %q", tc.name, tc.body, got, tc.want)
		}
	}

	// Truncation: long first line caps at inlineSummaryMaxRunes + ellipsis.
	long := strings.Repeat("word ", 100)
	got := deriveInlineSummary(long)
	if r := []rune(got); len(r) > inlineSummaryMaxRunes+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("expected summary truncated to <= %d runes ending in ellipsis, got %d runes: %q",
			inlineSummaryMaxRunes+1, len([]rune(got)), got)
	}
}
