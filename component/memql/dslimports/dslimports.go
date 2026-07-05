// Package dslimports is the integration layer that ties the
// parser, the path resolver, and the import-graph machinery
// together into one entry point the engine + validator CLI call.
//
// It exists as a separate package (rather than living in dslfs)
// so dslfs stays parser-free; dslimports depends on both dslfs
// and the language parser/ast, while dslfs only depends on the
// standard library.
package dslimports

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageCompiler "github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// Compile-time guarantee that the ast alias still exists.
var _ = (*languageAst.File)(nil)

// Tree is the loaded, validated state of a DSL root. It carries
// every parsed file, the resolved import graph in topo order, and
// the per-file alias tables that the symbol-resolution layer
// consumes.
//
// A Tree is the input to compilation + registration; the engine's
// Validate(target) API returns one of these (or a typed error if
// any layer failed).
type Tree struct {
	// Root is the fs.FS the tree was loaded from. Kept for any
	// downstream pass that needs to re-read file contents (e.g.
	// the diagnostic CLI when emitting source-context windows).
	Root fs.FS

	// Files maps each file's root-relative path to its parsed AST.
	// Files that failed to parse are not present; their errors are
	// surfaced via LoadError instead.
	Files map[string]*languageAst.File

	// Graph is the import dependency graph. Topo() returns the
	// compilation order, leaves-first.
	Graph *dslfs.ImportGraph

	// Aliases is the per-file resolved-import table.
	// Aliases[file][localAlias] = root-relative path of the imported file.
	Aliases map[string]dslfs.FileImports

	// Order is the topo-sorted load order (Graph.Topo() result),
	// memoized at load time so callers don't recompute.
	Order []string
}

// Load walks `root`, parses every `.memql` file, resolves imports,
// detects cycles, and returns the Tree.
//
// Errors are accumulated where reasonable (each file's parse error
// is independent; each file's bad imports are independent). The
// returned LoadError wraps every per-file diagnostic in a single
// value the validator CLI / engine startup can print as one report.
//
// If any error layer fires, the partial Tree is still returned (so
// the validator can produce useful suggestions on top of a partial
// load); callers should not register any of the Tree's contents
// into runtime registries when err != nil.
func Load(root fs.FS) (*Tree, error) {
	tree := &Tree{
		Root:    root,
		Files:   make(map[string]*languageAst.File),
		Aliases: make(map[string]dslfs.FileImports),
	}

	paths, err := dslfs.WalkMemqlFiles(root)
	if err != nil {
		return tree, fmt.Errorf("walking DSL root: %w", err)
	}

	var diagnostics []error
	rawByFile := make(map[string][]dslfs.RawImport, len(paths))

	for _, p := range paths {
		f, openErr := root.Open(p)
		if openErr != nil {
			diagnostics = append(diagnostics, fmt.Errorf("%s: open: %w", p, openErr))
			continue
		}
		content, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			diagnostics = append(diagnostics, fmt.Errorf("%s: read: %w", p, readErr))
			continue
		}

		// Run the full rewriter chain (struct-form spec / trait /
		// query / mutation / logic / automation / file-top args)
		// before invoking the bare parser. This is the same path
		// the engine takes at startup; running it here keeps
		// dslimports.Load semantically aligned with the runtime
		// loader so files the engine accepts also lint clean.
		fileAst, parseErr := languageCompiler.ParseFileSource(string(content))
		if parseErr != nil {
			// Two-tier fallback. shape / provider / builtin / prompt
			// / tool / policy files use dedicated runtime parsers
			// and don't go through the generic parser; after
			// StripNonProceduralBlocks removes their bodies the
			// remaining source is just comments + imports and
			// ParseFileSource returns the sentinel ErrEmptyInput.
			// That sentinel-shaped failure IS the legitimate fall-
			// back case: the file has no procedural content to
			// parse, so we accept the imports-only projection
			// without a diagnostic.
			//
			// EVERY OTHER parseErr is a real problem -- malformed
			// procedural content, file-level orphan tokens that
			// the rewriter chokes on (memql#293), broken syntax in
			// queries / mutations / logic / automations. Those MUST
			// surface as diagnostics so the lint gate catches them;
			// the prior unconditional swallow let three hours of
			// real bugs through this session (orphan logic-line
			// fragments in bff#55, broken
			// autoJoinAI hash lookup that became memql#276 / #273
			// Layer 1).
			//
			// We still produce the imports-only projection in both
			// branches so the cross-file integrity check downstream
			// can name a bad file's importers; without that, a single
			// malformed file would mask import errors in unrelated
			// files.
			treatAsDedicatedParserFile := errors.Is(parseErr, languageParser.ErrEmptyInput)

			importsOnly, importsErr := languageParser.ExtractImports(string(content))
			if importsErr != nil {
				diagnostics = append(diagnostics, fmt.Errorf("%s: parse: %w", p, parseErr))
				continue
			}
			if !treatAsDedicatedParserFile {
				diagnostics = append(diagnostics, fmt.Errorf("%s: parse: %w", p, parseErr))
			}
			importsOnly.Path = p
			tree.Files[p] = importsOnly
			raws := make([]dslfs.RawImport, 0, len(importsOnly.Imports))
			for _, imp := range importsOnly.Imports {
				raws = append(raws, dslfs.RawImport{Path: imp.Path, Alias: imp.Alias})
			}
			rawByFile[p] = raws
			continue
		}
		fileAst.Path = p
		tree.Files[p] = fileAst

		raws := make([]dslfs.RawImport, 0, len(fileAst.Imports))
		for _, imp := range fileAst.Imports {
			raws = append(raws, dslfs.RawImport{Path: imp.Path, Alias: imp.Alias})
		}
		rawByFile[p] = raws
	}

	graph, aliases, buildErr := dslfs.BuildImportGraph(rawByFile)
	tree.Graph = graph
	tree.Aliases = aliases
	if buildErr != nil {
		// BuildErrors already carries per-file detail; surface as-is
		// alongside the per-file parse/read diagnostics.
		diagnostics = append(diagnostics, buildErr)
	}

	// Add every walked file as a graph node so leaves with no
	// imports + no importers are still ordered (BuildImportGraph
	// only registers files that import or are imported).
	for _, p := range paths {
		tree.Graph.AddNode(p)
	}

	// Detect cycles + emit topo order.
	order, topoErr := tree.Graph.Topo()
	if topoErr != nil {
		diagnostics = append(diagnostics, topoErr)
	} else {
		tree.Order = order
	}

	// Verify every imported file actually exists in the tree.
	// Missing-target errors only surface after the build step so
	// the report names the importing file, not the missing file.
	for importer, fileAliases := range tree.Aliases {
		for alias, target := range fileAliases {
			if _, ok := tree.Files[target]; !ok {
				diagnostics = append(diagnostics, fmt.Errorf("%s: import %q (alias %q) targets %q which does not exist in the DSL root",
					importer, target, alias, target))
			}
		}
	}

	// Surface stable, deterministic error ordering.
	sort.SliceStable(diagnostics, func(i, j int) bool {
		return diagnostics[i].Error() < diagnostics[j].Error()
	})

	if len(diagnostics) > 0 {
		return tree, &LoadError{Diagnostics: diagnostics}
	}
	return tree, nil
}

// LoadError aggregates every per-file diagnostic from a single Load
// pass. The validator CLI prints these to the user; the engine
// startup hook fails the process loud when any error fires.
type LoadError struct {
	Diagnostics []error
}

func (e *LoadError) Error() string {
	if len(e.Diagnostics) == 0 {
		return "load: no errors"
	}
	s := fmt.Sprintf("load: %d diagnostic(s):\n", len(e.Diagnostics))
	for _, d := range e.Diagnostics {
		s += "  - " + d.Error() + "\n"
	}
	return s
}

// Unwrap exposes the underlying errors for errors.Is / errors.As.
func (e *LoadError) Unwrap() []error {
	return e.Diagnostics
}
