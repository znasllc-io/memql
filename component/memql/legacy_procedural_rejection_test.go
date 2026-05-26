package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memql#303 retired author-written procedural form. The DSL-load
// entry point (tryParseNewFunctionSyntax) calls
// parser.RejectLegacyProceduralAuthorForm before any rewriting runs;
// every author-written .memql slice in that shape fails fast.
//
// This test verifies the end-to-end loader path, not the regex
// alone -- the langparser package has its own per-receiver coverage
// (TestRejectLegacyProceduralAuthorForm_AllReceivers).

func TestTryParseNewFunctionSyntax_RejectsLegacyProcedural(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})

	src := `@description("legacy procedural form -- author surface retired in memql#303")
func (Query) legacyQueryName(ctx any) (any, error) {
  return concept==v1:cognition:space, nil
}`

	_, err := tryParseNewFunctionSyntax("legacyQueryName", "query", src, "test.memql", registry)
	if err == nil {
		t.Fatal("expected rejection for author-written procedural form")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Fatalf("error should mention retirement; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "memql#303") {
		t.Fatalf("error should cite the retirement issue; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Query") {
		t.Fatalf("error should name the receiver; got %q", err.Error())
	}
}
