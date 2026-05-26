package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// skipCountingHandler is a slog.Handler that tallies the unified
// function loader's "skipping slice" warnings, grouped by the `kind`
// attribute, so a conformance test can assert zero skips per construct
// kind without scraping formatted log text.
type skipCountingHandler struct {
	mu     *sync.Mutex
	counts map[string]int
	detail *[]string
}

func (h skipCountingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h skipCountingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h skipCountingHandler) WithGroup(string) slog.Handler            { return h }

func (h skipCountingHandler) Handle(_ context.Context, r slog.Record) error {
	if !strings.Contains(r.Message, "skipping slice") {
		return nil
	}
	var kind, fn, file, errStr string
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "kind":
			kind = a.Value.String()
		case "function":
			fn = a.Value.String()
		case "file":
			file = a.Value.String()
		case "error":
			errStr = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	h.counts[kind]++
	*h.detail = append(*h.detail, fmt.Sprintf("  %s :: %s (kind=%s) -> %s", file, fn, kind, errStr))
	h.mu.Unlock()
	return nil
}

// TestUnifiedLoaderZeroAutomationLogicSkips is the issue #212 acceptance
// conformance test: the embedded DSL must load through the unified
// function loader with ZERO skipped `automation` and `logic` slices.
//
// Background: automations are NOT function-registry constructs (they
// load via component/automations) so they must not be sliced here at
// all; logic slices must all parse + register. Regressions to watch
// for: the slicer mis-extracting a nested `logic NAME { ... }`
// invocation from inside an automation `step` block, or a malformed /
// returnless logic body.
//
// Query skips are also asserted zero as of #214 (query-prefix naming,
// retired @useConcept, the misnamed policyTrace concept). Mutation
// skips remain tracked separately (#213 args-usage + #215 workbench);
// tighten this to zero skips of ANY kind once those land.
func TestUnifiedLoaderZeroAutomationLogicSkips(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	var detail []string
	logger := slog.New(skipCountingHandler{mu: &mu, counts: counts, detail: &detail})

	// Concepts must load first so concept-binding resolution works
	// during function parsing.
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(quiet); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	autoSkips := counts[string(languageParser.FunctionTypeAutomation)]
	logicSkips := counts[string(languageParser.FunctionTypeLogic)]
	querySkips := counts[string(languageParser.FunctionTypeQuery)]
	if autoSkips != 0 || logicSkips != 0 || querySkips != 0 {
		t.Errorf("issues #212/#214: expected 0 automation + 0 logic + 0 query skipped slices, got automation=%d logic=%d query=%d. Skipped:\n%s",
			autoSkips, logicSkips, querySkips, strings.Join(detail, "\n"))
	}
}
