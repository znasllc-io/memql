package memql

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// skipCountingHandler is a slog.Handler that tallies the loaders'
// skip warnings, grouped by construct kind, so a conformance test can
// assert zero skips per kind without scraping formatted log text. It
// catches both the function loader's "skipping slice" warnings (kind
// from the `kind` attr) and the concept loader's "skipping concept"
// warning (recorded under kind="concept").
type skipCountingHandler struct {
	mu     *sync.Mutex
	counts map[string]int
	detail *[]string
}

func (h skipCountingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h skipCountingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h skipCountingHandler) WithGroup(string) slog.Handler            { return h }

func (h skipCountingHandler) Handle(_ context.Context, r slog.Record) error {
	isSlice := strings.Contains(r.Message, "skipping slice")
	isConcept := strings.Contains(r.Message, "skipping concept")
	if !isSlice && !isConcept {
		return nil
	}
	var kind, fn, file, concept, errStr string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "kind":
			kind = a.Value.String()
		case "function":
			fn = a.Value.String()
		case "file":
			file = a.Value.String()
		case "concept":
			concept = a.Value.String()
		case "error":
			errStr = a.Value.String()
		}
		return true
	})
	if isConcept {
		kind = "concept"
		if fn == "" {
			fn = concept
		}
	}
	h.mu.Lock()
	h.counts[kind]++
	*h.detail = append(*h.detail, fmt.Sprintf("  %s :: %s (kind=%s) -> %s", file, fn, kind, errStr))
	h.mu.Unlock()
	return nil
}

// TestUnifiedLoaderZeroSkips is the epic #218 acceptance conformance
// test: the embedded DSL must load through the concept loader and the
// unified function loader with ZERO skipped slices or concepts of ANY
// kind.
//
// History: this started life asserting only automation + logic skips
// (#212), then added query skips (#214), then mutation + concept skips
// once #213 (mutation args-usage) and #215 (workbench concept/mutation)
// landed. It now asserts the strong invariant -- any construct the
// migration left unloadable, of any kind, fails this test with the
// loader's own error message.
//
// Regressions to watch for: a concept declaring a reserved intrinsic
// (id/createdAt/createdBy/...), a mutation write block written bare
// (`update {` instead of `update <concept> {`), a query missing the
// `query` prefix, a logic body using the unsupported
// `name := if cond { multi-stmt }` form, or a `@useConcept(...)`
// annotation that should be a file-top `use` import.
func TestUnifiedLoaderZeroSkips(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	var detail []string
	logger := slog.New(skipCountingHandler{mu: &mu, counts: counts, detail: &detail})

	// Concepts must load first so concept-binding resolution works
	// during function parsing. Concept-build skips are captured by the
	// same handler (kind="concept").
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	total := 0
	kinds := make([]string, 0, len(counts))
	for k, n := range counts {
		total += n
		kinds = append(kinds, fmt.Sprintf("%s=%d", k, n))
	}
	if total != 0 {
		sort.Strings(kinds)
		t.Errorf("epic #218: expected 0 skipped slices/concepts of any kind, got %d (%s). Skipped:\n%s",
			total, strings.Join(kinds, " "), strings.Join(detail, "\n"))
	}
}
