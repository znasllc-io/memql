package memql

// unified_spec_loader.go walks the unified DSL tree, extracts every
// `spec NAME { ... }` and `trait NAME { ... }` declaration, parses
// each through the langparser's load-time path, and registers the
// resulting Spec. Specs + traits share the SpecRegistry (the IsTrait
// flag distinguishes them at runtime); two passes -- one per keyword
// -- run through the same shared baseloader pipeline.
//
// memql#334 (sub-epic #329 / #310 Stage 1C) migrated the parsing
// half off the hand-rolled spec parser onto
// languageParser.ParseSpecDecl + the in-package specDeclToSpec
// converter; the hand-rolled parser was deleted with memql#5359.

import (
	"fmt"
	"log/slog"
	"strings"

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
		anchoredExtractAdapter,
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
		anchoredExtractAdapter,
		parse,
		registry.add,
		sink,
	)
	rep.FoldSink("specs", traits, sink)
	return specs + traits, err
}

// anchoredExtractAdapter is extractAdapter with every slice anchored at its
// line in the file (languageParser.AnchorSource): ParseSpecDecl lexes the
// slice alone, and without the anchor a refusal inside a spec's lambda names
// the line within the slice -- line 2 for a spec whose doc comment is line 1
// -- instead of the file's (memql#5364). Only the parse sees the anchor; the
// registry keeps nothing of the slice's text.
func anchoredExtractAdapter(content, keyword string) []baseloader.Slice {
	src := constructDeclarationSlices(content, keyword)
	out := make([]baseloader.Slice, len(src))
	for i, s := range src {
		line := 1 + strings.Count(content[:s.Start], "\n")
		out[i] = baseloader.Slice{Name: s.Name, Source: languageParser.AnchorSource(s.Source, line)}
	}
	return out
}
