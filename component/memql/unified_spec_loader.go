package memql

// unified_spec_loader.go walks the unified DSL tree, extracts every
// `spec NAME { ... }` and `trait NAME { ... }` declaration, parses
// each through the langparser's load-time path, and registers the
// resulting Spec. Specs + traits share the SpecRegistry (the IsTrait
// flag distinguishes them at runtime); two passes -- one per keyword
// -- run through the same shared baseloader pipeline.
//
// memql#334 (sub-epic #329 / #310 Stage 1C) migrated the parsing
// half off the hand-rolled parseSpecMemQL onto
// languageParser.ParseSpecDecl + the in-package specDeclToSpec
// converter. The hand-rolled parser is unreferenced from production
// after this child; spec_parser_test.go still exercises it pending
// the final deletion in sub-epic #329's cleanup PR.

import (
	"fmt"
	"log/slog"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// LoadUnifiedSpecs walks dsl.Tree() and registers every spec + trait
// found across `<domain>/specs.memql` and `<domain>/traits.memql`.
//
// Returns (specCount + traitCount, error). Errors from individual
// slices are logged at WARN (via the shared baseloader pipeline) +
// skipped -- one bad slice should not blank-out the rest of the tree,
// but the skip is surfaced loudly so a malformed spec/trait can't rot
// silently (memql#2356).
func LoadUnifiedSpecs(logger *slog.Logger, registry *SpecRegistry, report ...*LoadReport) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("spec registry is nil")
	}
	files := baseloader.ReadAll(logger)

	parse := func(origin string, raw []byte) (*Spec, error) {
		decl, err := languageParser.ParseSpecDecl(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", origin, err)
		}
		spec, err := specDeclToSpec(decl, origin)
		if err != nil {
			return nil, err
		}
		// nil, nil = @disabled (the intentional-skip contract). Reserve
		// the name: promotion guards refuse it and diagnostics say
		// "disabled" instead of "not found" (#2607).
		if spec == nil {
			registry.MarkDisabled(decl.Name)
		}
		return spec, nil
	}

	rep := firstReport(report)
	sink := newBaseloaderSink()
	specs, err := baseloader.LoadOne[Spec](
		logger,
		"memql.unifiedSpecLoader",
		"spec",
		files,
		extractAdapter,
		parse,
		registry.add,
		sink,
	)
	rep.FoldSink("specs", specs, sink)
	if err != nil {
		return specs, err
	}
	sink = newBaseloaderSink()
	traits, err := baseloader.LoadOne[Spec](
		logger,
		"memql.unifiedSpecLoader",
		"trait",
		files,
		extractAdapter,
		parse,
		registry.add,
		sink,
	)
	rep.FoldSink("specs", traits, sink)
	return specs + traits, err
}
