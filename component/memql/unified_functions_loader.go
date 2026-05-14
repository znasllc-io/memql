package memql

// unified_functions_loader.go is the function-loader companion to
// unified_loader.go (concepts) and unified_loader_test.go (tests).
// It walks the new domain-first DSL tree at dsl.Tree(), extracts
// each function declaration from the consolidated files, parses
// each slice through the legacy single-function pipeline
// (tryParseNewFunctionSyntax), and upserts the resulting Function
// into the engine's FunctionRegistry.
//
// During the transitional state of Pass 2, this runs alongside
// the legacy function_loader. Each function appears in both load
// paths; Upsert overwrites the legacy entry with the unified
// entry. Functions defined only in the new tree (none today, but
// the path is ready) get registered. Functions only in legacy
// keep working through the legacy path.

import (
	"fmt"
	"log/slog"

	memoryNodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	languageParser "github.com/visionarys-io/memql/component/language/parser"
	"github.com/visionarys-io/memql/component/memql/baseloader"
)

// LoadUnifiedFunctions walks the unified DSL tree, extracts every
// function-shaped declaration (query / mutation / spec / logic /
// automation / and the procedural form for shape / tool / builtin /
// prompt / provider / policy), parses each as a single-function
// source, and upserts into the supplied FunctionRegistry.
//
// Returns the number of functions registered + the per-kind counts
// for observability. Errors from individual function parses are
// logged + skipped (best-effort registration; legacy loader covers
// any gaps).
func LoadUnifiedFunctions(logger *slog.Logger, registry *FunctionRegistry, conceptRegistry memoryNodes.Registry) (int, map[languageParser.FunctionType]int, error) {
	if registry == nil {
		return 0, nil, fmt.Errorf("function registry is nil")
	}

	counts := make(map[languageParser.FunctionType]int)
	total := 0

	for _, raw := range baseloader.ReadAll(logger) {
		slices := ExtractFunctionSlices(raw.Content)
		if len(slices) == 0 {
			continue
		}

		for _, slice := range slices {
			fn, parseErr := dispatchPerConstructParser(slice, "unified:"+raw.Path, conceptRegistry)
			if parseErr != nil {
				if logger != nil {
					logger.Debug("unified function loader: skipping slice that failed to parse",
						"component", "memql.unifiedFunctionLoader",
						"file", raw.Path,
						"function", slice.Name,
						"kind", slice.Kind,
						"error", parseErr)
				}
				continue
			}
			if fn == nil {
				continue
			}

			if upsertErr := registry.Upsert(fn); upsertErr != nil {
				if logger != nil {
					logger.Warn("unified function loader: upsert failed",
						"component", "memql.unifiedFunctionLoader",
						"file", raw.Path,
						"function", slice.Name,
						"error", upsertErr)
				}
				continue
			}

			counts[slice.Kind]++
			total++
		}
	}

	if logger != nil {
		logger.Info("unified function loader: registered functions",
			"component", "memql.unifiedFunctionLoader",
			"total", total,
			"counts", fmt.Sprintf("%v", counts))
	}

	return total, counts, nil
}

// dispatchPerConstructParser routes each function slice to the
// dedicated parser for its construct kind. Each dedicated parser
// validates its construct's annotation surface (hard-rejecting
// unknown annotations / typos) before delegating structural parsing
// to the shared tryParseNewFunctionSyntax helper.
//
// Procedural-form receivers (`func (Kind) NAME(...)` -- legacy
// surface) flow through the shared helper directly, since no DSL
// author writes them anymore and the per-construct annotation
// surface only applies to the canonical struct form.
func dispatchPerConstructParser(slice FunctionSlice, origin string, conceptRegistry memoryNodes.Registry) (*Function, error) {
	switch slice.Kind {
	case languageParser.FunctionTypeQuery:
		return parseQueryMemQL(slice.Name, origin, slice.Source, conceptRegistry)
	case languageParser.FunctionTypeMutation:
		return parseMutationMemQL(slice.Name, origin, slice.Source, conceptRegistry)
	case languageParser.FunctionTypeLogic:
		return parseLogicMemQL(slice.Name, origin, slice.Source, conceptRegistry)
	case languageParser.FunctionTypeAutomation:
		return parseAutomationMemQL(slice.Name, origin, slice.Source, conceptRegistry)
	default:
		// Other kinds (shape / tool / builtin / prompt / provider /
		// policy) have their own dedicated parsers + loaders. Their
		// slices shouldn't reach this dispatcher today, but route
		// them through the shared helper as a safety net.
		return tryParseNewFunctionSyntax(slice.Name, string(slice.Kind), slice.Source, origin, conceptRegistry)
	}
}
