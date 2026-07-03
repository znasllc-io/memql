package memql

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"text/template"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestEmbeddedDSLLoadsCleanly is the #282 / epic #218 DoD #1 guard:
// every construct in the embedded DSL tree must load without
// triggering a "skipping" / "failed to parse" / "failed to compile"
// warning from any production loader. The next contributor who adds
// an automation / logic / mutation / query / shape / spec / tool /
// prompt / provider / policy / builtin / seed slice with a subtle
// syntax or annotation issue silently regressing the system to "76
// skipped constructs" (the failure mode that produced epic #218) is
// caught here in CI instead of on a bff startup nobody is watching.
//
// Mechanics:
//
//  1. A captureHandler slog.Handler collects every WARN+ record the
//     loaders emit.
//  2. We call the same loaders bff bootstraps (see engine_bootstrap.go's
//     Init -> per-construct load sequence) in the same order, with
//     fresh registries.
//  3. After everything has loaded, we walk the captured records and
//     fail the test on any whose message names a skip / parse-fail /
//     compile-fail / upsert-fail. The failure message includes the
//     offending file + construct name + reason so a regression has a
//     clean repro.
//
// Why a log-capture handler instead of a structured "skipped" slice
// on the loaders: the loaders already log richly, and adding parallel
// structured tracking is duplicate plumbing. The handler-based
// assertion is also the most faithful reproduction of what an
// operator sees on `bff` startup -- if it appears in the log, it
// fails the test.
func TestEmbeddedDSLLoadsCleanly(t *testing.T) {
	capture := newCaptureHandler()
	logger := slog.New(capture)

	// Load concepts first -- every other construct binds against the
	// concept registry. LoadUnifiedConcepts merges into the
	// memoryNodes package's global registry, so subsequent loaders
	// pick them up via the same merge target.
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	// Concepts loaded above merged into memoryNodes.DefaultRegistry()
	// -- the shared concept registry every binding-aware loader
	// resolves against. Pass it through where the loader signature
	// asks for one.
	concepts := memoryNodes.DefaultRegistry()

	// Specs (legacy + unified). Build the schema index off the
	// loaded concept set so spec-loader binding checks work.
	specSchemaIdx, err := buildSchemaIndex(concepts)
	if err != nil {
		t.Fatalf("buildSchemaIndex: %v", err)
	}
	specRegistry, err := loadEmbeddedSpecs(logger, specSchemaIdx)
	if err != nil {
		t.Fatalf("loadEmbeddedSpecs: %v", err)
	}
	if _, err := LoadUnifiedSpecs(logger, specRegistry); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}

	// Functions (queries / mutations / logic / automations).
	functionRegistry, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, err := LoadUnifiedFunctions(logger, functionRegistry, concepts); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}

	// Shapes.
	shapeRegistry, err := loadEmbeddedShapes(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedShapes: %v", err)
	}
	if _, err := LoadUnifiedShapes(logger, shapeRegistry); err != nil {
		t.Fatalf("LoadUnifiedShapes: %v", err)
	}

	// Builtins overlay onto the function registry.
	if _, err := LoadUnifiedBuiltins(logger, functionRegistry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}

	// Tools.
	toolRegistry, err := loadToolRegistry(logger)
	if err != nil {
		t.Fatalf("loadToolRegistry: %v", err)
	}
	if _, err := LoadUnifiedTools(logger, toolRegistry); err != nil {
		t.Fatalf("LoadUnifiedTools: %v", err)
	}

	// Prompts. Use an empty partials template since this test
	// doesn't exercise prompt rendering; the loader only needs a
	// non-nil partials root to parse @templateFile references.
	promptRegistry, err := loadPromptRegistry(logger)
	if err != nil {
		t.Fatalf("loadPromptRegistry: %v", err)
	}
	if _, err := LoadUnifiedPrompts(logger, promptRegistry, template.New("partials")); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}

	// Providers + Seeds + Policies + PolicyFunctions.
	providerRegistry, err := loadAIProviders(logger)
	if err != nil {
		t.Fatalf("loadAIProviders: %v", err)
	}
	if _, err := LoadUnifiedProviders(logger, providerRegistry); err != nil {
		t.Fatalf("LoadUnifiedProviders: %v", err)
	}

	seedRegistry := NewSeedRegistry()
	if _, err := LoadUnifiedSeeds(logger, seedRegistry); err != nil {
		t.Fatalf("LoadUnifiedSeeds: %v", err)
	}

	policyRegistry, err := loadAIPolicies(logger)
	if err != nil {
		t.Fatalf("loadAIPolicies: %v", err)
	}
	if _, err := LoadUnifiedPolicies(logger, policyRegistry); err != nil {
		t.Fatalf("LoadUnifiedPolicies: %v", err)
	}

	// Assert: no captured record names a skip / fail. The matcher
	// is intentionally broad -- any future loader that adds a new
	// "couldn't register XYZ" message shape catches automatically.
	bad := capture.skipLike()
	if len(bad) == 0 {
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("embedded DSL load is not clean -- %d skip/fail messages captured:\n", len(bad)))
	const maxShown = 20
	for i, rec := range bad {
		if i >= maxShown {
			sb.WriteString(fmt.Sprintf("  ... (%d more)\n", len(bad)-maxShown))
			break
		}
		sb.WriteString("  ")
		sb.WriteString(rec.format())
		sb.WriteString("\n")
	}
	sb.WriteString("\nResolve each (fix the construct or update the loader's accept-path) so the embedded tree loads with zero warnings. Epic #218 / issue #282.")
	t.Error(sb.String())
}

// captureHandler is a minimal slog.Handler that records every WARN+
// log record in memory. Records below WARN are dropped -- the
// "skipping" / "failed" surfaces every loader uses are all at WARN
// or ERROR.
type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
	attrs   []slog.Attr
}

type capturedRecord struct {
	Level   slog.Level
	Message string
	Attrs   map[string]any
}

func newCaptureHandler() *captureHandler {
	return &captureHandler{}
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, capturedRecord{
		Level:   r.Level,
		Message: r.Message,
		Attrs:   attrs,
	})
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &captureHandler{records: h.records, attrs: merged}
}

func (h *captureHandler) WithGroup(_ string) slog.Handler { return h }

// skipLike returns the subset of captured records whose message
// names a loader-side skip / parse fail / compile fail / upsert
// fail. The match is intentionally broad: any new loader that
// invents a "couldn't register" phrase is caught next time it
// fires.
func (h *captureHandler) skipLike() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []capturedRecord
	for _, rec := range h.records {
		msg := strings.ToLower(rec.Message)
		// "expression-only" / "single-statement form" / similar
		// don't indicate a skipped slice, only a fallback path -- so
		// the matcher requires one of these load-skipping verbs.
		//
		// memql#2356 raised the per-construct parse/convert/register
		// failures in baseloader + the unified spec/kinds/policy loaders
		// from Debug to Warn. Those emit the trailing-verb shapes
		// "<loader>: parse failed" / "convert failed" / "register
		// failed" / "compile failed", so the matcher recognises them
		// explicitly -- a malformed spec/trait/shape/builtin/prompt/
		// provider/policy now trips this gate instead of hiding at Debug.
		//
		// NOT matched on purpose: the provider loader's environment-
		// dependent "auth resolution failed; registered as unavailable"
		// and "provider client construction failed; registered as
		// unavailable" -- those register a degraded-but-present provider
		// (missing API key in the test env), not a dropped construct.
		if strings.Contains(msg, "skip") ||
			strings.Contains(msg, "failed to parse") ||
			strings.Contains(msg, "failed to compile") ||
			strings.Contains(msg, "failed to build") ||
			strings.Contains(msg, "function skipped") ||
			strings.Contains(msg, "parse failed") ||
			strings.Contains(msg, "convert failed") ||
			strings.Contains(msg, "register failed") ||
			strings.Contains(msg, "compile failed") ||
			strings.Contains(msg, "upsert failed") ||
			strings.Contains(msg, "rejected") {
			out = append(out, rec)
		}
	}
	return out
}

func (r capturedRecord) format() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[%s] %s", r.Level, r.Message))
	if file, ok := r.Attrs["file"].(string); ok && file != "" {
		sb.WriteString(" file=" + file)
	}
	for _, key := range []string{"function", "concept", "shape", "spec", "tool", "prompt", "provider", "seed", "policy", "kind", "name", "error"} {
		if v, ok := r.Attrs[key]; ok {
			sb.WriteString(fmt.Sprintf(" %s=%v", key, v))
		}
	}
	return sb.String()
}
