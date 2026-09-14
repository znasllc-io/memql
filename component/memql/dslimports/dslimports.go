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
	"github.com/znasllc-io/memql/core/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
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

	// ImportsOnly marks files whose AST is the imports-only fallback
	// projection (comment-only files, or content the generic parser
	// cannot see) -- their Definitions are empty by construction, so
	// referential-integrity passes must not treat a symbol's absence
	// from them as an error.
	ImportsOnly map[string]bool
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
		Root:        root,
		Files:       make(map[string]*languageAst.File),
		Aliases:     make(map[string]dslfs.FileImports),
		ImportsOnly: make(map[string]bool),
	}

	paths, err := dslfs.WalkMemqlFiles(root)
	if err != nil {
		return tree, fmt.Errorf("walking DSL root: %w", err)
	}

	var diagnostics []error
	rawByFile := make(map[string][]dslfs.RawImport, len(paths))

	// Each file is parsed through the front end of the edition its domain
	// declares (memql#5358), exactly as the engine reads it at boot, so a
	// tree written in another edition lints as it loads.
	//
	// A domain whose language line the engine refuses refuses boot, so Load
	// reports it: one LanguageLineError per problem the resolver found,
	// carrying the resolver's message and code. A caller that runs Load alone
	// -- memql-cockpit's `memql lint` -- would otherwise be told a tree that
	// refuses boot is clean. (memqllint also runs the engine-parity pass,
	// which reports the same refusal, and prints it once.)
	//
	// The refused domain is read by no loader at boot, so it is not parsed
	// here either: its files enter the tree OPAQUE -- present, importing
	// nothing, defining nothing, marked ImportsOnly -- with no diagnostic of
	// their own. An importer in another domain still resolves the module
	// rather than cascading a "does not exist" off a file that is merely
	// unread.
	lines, lineProblems := languageParser.ResolveLanguageLines(root, memqldsl.EmbeddedTree{})
	for _, p := range lineProblems {
		diagnostics = append(diagnostics, &LanguageLineError{Problem: p})
	}

	for _, p := range paths {
		if line, ok := lines.For(p); ok && line.Refused {
			tree.Files[p] = &languageAst.File{Path: p}
			tree.ImportsOnly[p] = true
			continue
		}
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
		content, prepErr := lines.Prepare(p, content)
		if prepErr != nil {
			diagnostics = append(diagnostics, fmt.Errorf("%s: %w", p, prepErr))
			continue
		}

		// A top-level statement no construct keyword opens -- a typo'd
		// `qurey`, the retired `import ( ... )` block, which the parser
		// still reads -- loads as nothing, and boot refuses it (the
		// construct-keyword gate, dslgate). Load runs the same check over
		// the same text, so a caller that runs Load alone (memql-cockpit's
		// `memql lint`) sees what boot refuses.
		unknown := languageParser.FindUnknownConstructKeywords(string(content))
		for _, u := range unknown {
			diagnostics = append(diagnostics, &ConstructKeywordError{File: p, Refusal: u})
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
			//
			// The parser raises the construct-keyword refusal too, for the
			// first such statement it meets; that copy is the one above
			// again, so it is not reported twice.
			treatAsDedicatedParserFile := errors.Is(parseErr, languageParser.ErrEmptyInput)
			echo := refusedAbove(parseErr, unknown)

			importsOnly, importsErr := languageParser.ExtractImports(string(content))
			if importsErr != nil {
				if !echo {
					diagnostics = append(diagnostics, &FileParseError{File: p, Err: parseErr})
				}
				continue
			}
			if !treatAsDedicatedParserFile && !echo {
				diagnostics = append(diagnostics, &FileParseError{File: p, Err: parseErr})
			}
			importsOnly.Path = p
			tree.Files[p] = importsOnly
			tree.ImportsOnly[p] = true
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

// LanguageLineError is one refusal of a domain's language line
// (memql#5357): the domain declares none, a malformed one, one newer or
// older than this engine speaks, or an edition it has no front end for. Its
// text is the resolver's message, which names the file to fix and ends with
// the stable code ("[language_line_missing]"); Problem carries the domain,
// the file and the code for a caller that wants them without reading text.
type LanguageLineError struct {
	Problem languageParser.LanguageLineProblem
}

func (e *LanguageLineError) Error() string { return e.Problem.Message }

// ConstructKeywordError is one top-level statement of a file opened by a word
// no construct is spelled with (parser.FindUnknownConstructKeywords), which
// boot refuses. Its text names the file and the statement's line, then the
// refusal, which ends with its code ("[construct_unknown]").
type ConstructKeywordError struct {
	File    string
	Refusal languageParser.UnknownConstructKeyword
}

func (e *ConstructKeywordError) Error() string {
	return fmt.Sprintf("%s: line %d: %s", e.File, e.Refusal.Line, e.Refusal.Message)
}

// Unwrap exposes the refusal for errors.As.
func (e *ConstructKeywordError) Unwrap() error { return &e.Refusal }

// refusedAbove reports whether parseErr is the parser's copy of one of the
// file's construct-keyword refusals: the same word, refused on the same line.
func refusedAbove(parseErr error, unknown []languageParser.UnknownConstructKeyword) bool {
	var u *languageParser.UnknownConstructKeyword
	if !errors.As(parseErr, &u) {
		return false
	}
	for _, k := range unknown {
		if k.Keyword == u.Keyword && k.Line == u.Line {
			return true
		}
	}
	return false
}

// FileParseError is one file of the tree the parser refused. Its text is the
// one Load has always printed ("<file>: parse: <the parser's error>"), whose
// position is the file's own line (compiler.ParseFileSource); the type lets a
// caller read the file, and reach the parser's own error and its cause,
// without taking the text apart.
type FileParseError struct {
	File string
	Err  error
}

func (e *FileParseError) Error() string { return e.File + ": parse: " + e.Err.Error() }

// Unwrap exposes the parser's error for errors.Is / errors.As.
func (e *FileParseError) Unwrap() error { return e.Err }

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
