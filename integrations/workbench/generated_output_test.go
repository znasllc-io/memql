package workbench

import (
	"context"
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
		"notes.md":        "markdown",
		"a/b/report.MD":   "markdown",
		"slides.pdf":      "pdf",
		"memo.docx":       "document",
		"data.csv":        "spreadsheet",
		"sheet.xlsx":      "spreadsheet",
		"diagram.png":     "image",
		"photo.JPEG":      "image",
		"out.txt":         "text",
		"server.go":       "text",
		"config.yaml":     "text",
		"noextension":     "text",
		"archive.tar.gz":  "other",
		"blob.bin":        "other",
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
		map[string]any{"path": "/ws/out.txt"})
}

func TestPromoteWorkbenchOutput_ResolvesPlanOwner(t *testing.T) {
	ce := &captureEngine{}
	i := &Integration{}
	i.SetEngine(ce)

	i.promoteWorkbenchOutput(context.Background(), "plan-1", "agent-1",
		map[string]any{"path": "/ws/out.txt"})

	if len(ce.queries) != 1 {
		t.Fatalf("expected exactly one queryPlanById call, got %d: %v", len(ce.queries), ce.queries)
	}
	if !strings.HasPrefix(ce.queries[0], "queryPlanById(") {
		t.Errorf("expected queryPlanById, got %q", ce.queries[0])
	}
	if !strings.Contains(ce.queries[0], "plan-1") {
		t.Errorf("queryPlanById missing planId: %q", ce.queries[0])
	}
}

func TestPromoteWorkbenchOutput_SkipsWhenNoPath(t *testing.T) {
	ce := &captureEngine{}
	i := &Integration{}
	i.SetEngine(ce)

	i.promoteWorkbenchOutput(context.Background(), "plan-1", "agent-1", map[string]any{})
	if len(ce.queries) != 0 {
		t.Errorf("missing path should skip all engine calls, got %v", ce.queries)
	}
}
