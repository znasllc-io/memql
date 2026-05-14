package memql

// unified_spec_loader.go walks the unified DSL tree, extracts every
// `spec NAME { ... }` and `trait NAME { ... }` declaration, parses
// each via parseSpecMemQL, and registers the resulting Spec. Specs +
// traits share the SpecRegistry (the IsTrait flag distinguishes them
// at runtime); two passes -- one per keyword -- run through the same
// shared baseloader pipeline.

import (
	"fmt"
	"log/slog"

	"github.com/visionarys-io/memql/component/memql/baseloader"
)

// LoadUnifiedSpecs walks dsl.Tree() and registers every spec + trait
// found across `<domain>/specs.memql` and `<domain>/traits.memql`.
//
// Returns (specCount + traitCount, error). Errors from individual
// slices are logged at debug + skipped -- one bad slice should not
// blank-out the rest of the tree.
func LoadUnifiedSpecs(logger *slog.Logger, registry *SpecRegistry) (int, error) {
	if registry == nil {
		return 0, fmt.Errorf("spec registry is nil")
	}
	files := baseloader.ReadAll(logger)

	specs, err := baseloader.LoadOne[Spec](
		logger,
		"memql.unifiedSpecLoader",
		"spec",
		files,
		extractAdapter,
		parseSpecMemQL,
		registry.add,
	)
	if err != nil {
		return specs, err
	}
	traits, err := baseloader.LoadOne[Spec](
		logger,
		"memql.unifiedSpecLoader",
		"trait",
		files,
		extractAdapter,
		parseSpecMemQL,
		registry.add,
	)
	return specs + traits, err
}
