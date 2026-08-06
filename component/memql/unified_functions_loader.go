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

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/core/baseparser"
)

// constructAnnotationAllowLists keys each migrated construct's
// annotation allow-list by its FunctionType so dispatchPerConstructParser
// can validate annotations inline before delegating structural parsing
// to the langparser-backed tryParseNewFunctionSyntax. The four
// per-construct entry points (parseQueryMemQL / parseMutationMemQL /
// parseLogicMemQL / parseAutomationMemQL) were thin shells around the
// same validation + delegation; #308 inlined them. The four shells
// + their tests are deleted in sub-epic #306 child D.
var constructAnnotationAllowLists = map[languageParser.FunctionType]map[string]bool{
	languageParser.FunctionTypeQuery:      allowedQueryAnnotations,      // query_parser.go
	languageParser.FunctionTypeMutation:   allowedMutationAnnotations,   // mutation_parser.go
	languageParser.FunctionTypeLogic:      allowedLogicAnnotations,      // logic_parser.go
	languageParser.FunctionTypeAutomation: allowedAutomationAnnotations, // automation_parser.go
}

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
func LoadUnifiedFunctions(logger *slog.Logger, registry *FunctionRegistry, conceptRegistry memoryNodes.Registry, report ...*LoadReport) (int, map[languageParser.FunctionType]int, error) {
	if registry == nil {
		return 0, nil, fmt.Errorf("function registry is nil")
	}
	rep := firstReport(report)

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
					logger.Warn("unified function loader: skipping slice that failed to parse",
						"component", "memql.unifiedFunctionLoader",
						"file", raw.Path,
						"function", slice.Name,
						"kind", slice.Kind,
						"error", parseErr)
				}
				rep.AddSkip(baseloader.Skip{Component: "memql.unifiedFunctionLoader", Keyword: string(slice.Kind), Name: slice.Name, File: raw.Path, Phase: "parse", Err: parseErr.Error()})
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
				rep.AddSkip(baseloader.Skip{Component: "memql.unifiedFunctionLoader", Keyword: string(slice.Kind), Name: slice.Name, File: raw.Path, Phase: "register", Err: upsertErr.Error()})
				continue
			}

			counts[slice.Kind]++
			total++
		}
	}
	rep.AddRegistered("functions", total)

	if logger != nil {
		logger.Info("unified function loader: registered functions",
			"component", "memql.unifiedFunctionLoader",
			"total", total,
			"counts", fmt.Sprintf("%v", counts))
	}

	return total, counts, nil
}

// dispatchPerConstructParser routes each function slice through the
// langparser-backed shared parser (tryParseNewFunctionSyntax), with
// a per-construct annotation allow-list applied inline beforehand.
// Unknown / typo annotations on a query / mutation / logic /
// automation slice are hard-rejected here; structural parsing is the
// langparser's responsibility downstream.
//
// Procedural-form receivers (`func (Kind) NAME(...)` -- legacy
// surface) and the other construct kinds (shape / tool / builtin /
// prompt / provider / policy, parsed by their own dedicated loaders)
// reach this dispatcher only through the safety-net fall-through and
// skip the allow-list step.
func dispatchPerConstructParser(slice FunctionSlice, origin string, conceptRegistry memoryNodes.Registry) (*Function, error) {
	if allowed, ok := constructAnnotationAllowLists[slice.Kind]; ok {
		if err := baseparser.ValidateConstructAnnotations(slice.Source, string(slice.Kind), allowed); err != nil {
			return nil, fmt.Errorf("%s: %w", origin, err)
		}
	}
	return tryParseNewFunctionSyntax(slice.Name, string(slice.Kind), slice.Source, origin, conceptRegistry)
}
