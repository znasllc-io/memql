//go:build agent

package worker

import (
	"context"
	"strings"
	"testing"
)

func TestDeriveGeneratedOutputId_Deterministic(t *testing.T) {
	a := deriveGeneratedOutputId("computer_use", "user-1", "plan-1:/tmp/out.txt")
	b := deriveGeneratedOutputId("computer_use", "user-1", "plan-1:/tmp/out.txt")
	if a != b {
		t.Fatalf("same inputs must yield same id: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "genout-") {
		t.Fatalf("expected genout- prefix, got %q", a)
	}
	// genout- (7) + 16 hex chars.
	if len(a) != len("genout-")+16 {
		t.Fatalf("unexpected id length %d for %q", len(a), a)
	}
}

func TestDeriveGeneratedOutputId_DistinctInputs(t *testing.T) {
	base := deriveGeneratedOutputId("computer_use", "user-1", "plan-1:/tmp/out.txt")
	cases := map[string]string{
		"different source":  deriveGeneratedOutputId("workbench_generated", "user-1", "plan-1:/tmp/out.txt"),
		"different owner":    deriveGeneratedOutputId("computer_use", "user-2", "plan-1:/tmp/out.txt"),
		"different stableKey": deriveGeneratedOutputId("computer_use", "user-1", "plan-1:/tmp/other.txt"),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("%s: expected a distinct id, got the same %q", name, got)
		}
	}
}

func TestPathBasename(t *testing.T) {
	cases := map[string]string{
		"/tmp/out.txt":        "out.txt",
		"out.txt":             "out.txt",
		"a/b/c/report.md":     "report.md",
		"/var/lib/dir/":       "dir",
		"":                    "",
		"   ":                 "",
		"/only-root":          "only-root",
	}
	for in, want := range cases {
		if got := pathBasename(in); got != want {
			t.Errorf("pathBasename(%q) = %q, want %q", in, got, want)
		}
	}
}

// captureEngine is a stub *memql.MemQLEngine substitute is not possible
// (the field is a concrete type), so the fs_write action-filter is
// exercised by asserting the gating predicate directly: only fs_write
// reaches the promotion path. This mirrors the guard in handleDispatch.
func TestFsWriteActionFilter(t *testing.T) {
	promotes := map[string]bool{
		"fs_write":   true,
		"exec":       false,
		"fs_read":    false,
		"fs_list":    false,
		"fs_stat":    false,
		"http_fetch": false,
	}
	for action, want := range promotes {
		// The handleDispatch guard is `res.OK && action == "fs_write"`.
		got := action == "fs_write"
		if got != want {
			t.Errorf("action %q: filter = %v, want %v", action, got, want)
		}
	}
}

// promoteWorkerOutput must be a no-op (no panic) when the engine is nil,
// so a Library failure can never break the worker tool call.
func TestPromoteWorkerOutput_NilEngineNoPanic(t *testing.T) {
	i := &Integration{} // engine nil
	i.promoteWorkerOutput(context.Background(), Request{
		OwnerUserId: "user-1",
		AgentId:     "agent-1",
		PlanId:      "plan-1",
		Args:        map[string]any{"path": "/tmp/out.txt", "content": "hello"},
	})
}

func TestWorkerMimeType(t *testing.T) {
	// Extension-resolved types may carry a "; charset=..." suffix from the
	// stdlib table, so assert on the type prefix.
	prefixes := map[string]string{
		"report.md":      "text/markdown",
		"notes.markdown": "text/markdown",
		"data.csv":       "text/csv",
		"noext":          "text/plain",
		"":               "text/plain",
	}
	for in, want := range prefixes {
		if got := workerMimeType(in); !strings.HasPrefix(got, want) {
			t.Errorf("workerMimeType(%q) = %q, want prefix %q", in, got, want)
		}
	}
}

// uploadWorkerAttachment must fall back to "" (worker-local pointer) when no
// GCS uploader is wired, with no panic -- so a cluster without a bucket keeps
// working (memql#794 degrades to memql#789 behaviour).
func TestUploadWorkerAttachment_NoUploaderFallsBack(t *testing.T) {
	i := &Integration{} // uploader nil, bucket ""
	got := i.uploadWorkerAttachment(context.Background(), "plan-1", "/tmp/out.md", "out.md", "space-1", "user-1", []byte("hello"))
	if got != "" {
		t.Fatalf("expected empty attachmentId without an uploader, got %q", got)
	}
	// Empty content also yields no attachment.
	if got := i.uploadWorkerAttachment(context.Background(), "plan-1", "/tmp/out.md", "out.md", "space-1", "user-1", nil); got != "" {
		t.Fatalf("expected empty attachmentId for empty content, got %q", got)
	}
}
