package memql

// unified_loader.go is Pass 2 of the DSL restructure migration
// (docs/internal/design/dsl-import-model-refactor.md). It walks the new domain-first
// tree at dsl.Tree(), parses each .memql file via dslimports.Load,
// and dispatches every top-level declaration to its appropriate
// per-kind registration function.
//
// During the transitional state, the unified loader runs ALONGSIDE
// the legacy per-kind loaders (concept_loader, function_loader, ...).
// Duplicate registrations are harmless because:
//   - Concept IDs assemble identically (byte-equality verified by
//     Commit 2 step 1's migration audit), so the global concept
//     registry's ReplaceAll-style semantics handle the duplicate
//     gracefully.
//   - Function / shape / prompt registries use the same names,
//     so the second registration replaces the first.
//
// When Pass 2 is fully wired, the legacy loaders get retired and
// this becomes the only path.

import (
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageAst "github.com/znasllc-io/memql/component/language/ast"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// LoadUnifiedConcepts walks the unified DSL tree at dsl.Tree(),
// finds every ConceptDecl in every file, assembles each concept's
// ID from its @version + @namespace + declaration name, and
// registers them in the supplied memoryNodes registry.
//
// Every binary loads every concept -- the per-node @visibility
// filtering this loader used to do was removed during the genesis
// cleanup. Build tags still gate runtime integrations; DSL surface
// is uniform across all node types.
//
// Returns the number of concepts loaded + any errors accumulated
// across files. Concepts that fail to build are skipped so that one
// bad concept cannot stop a node booting -- but the skip is RETURNED
// as well as logged (memql#2909). A dropped concept takes every query,
// mutation and shape bound to it with it, and a caller that only sees
// a log line cannot gate on that: memqllint's engine-parity pass ran
// this very code and discarded the answer, so a product bundle with a
// mistyped property linted clean and lost a concept at boot.
//
// Implementation note: instead of going through dslimports.Load
// (which runs the full struct-form rewriter chain and gets
// confused by multi-concept consolidated files), we use the
// concepts-only extractor (ExtractConceptDecls) which scans each
// file's source text for `concept ... { }` blocks and parses each
// in isolation. This bypasses the rewriter limitation and gets
// us to full concept coverage from the new tree.
// ConceptSkip is one concept the unified loader could not build and
// therefore did not register. It exists so a caller can gate on a
// dropped concept rather than having to scrape a log (memql#2909).
type ConceptSkip struct {
	File    string // origin path in the DSL tree
	Concept string // canonical id the concept would have had
	Err     error  // why the schema build failed
}

// String leads with the CONSEQUENCE rather than the cause. The wrapped error
// already names the concept and the offending property; what it does not say,
// and what the author needs first, is that the whole concept is gone -- not
// the property (memql#2909).
func (s ConceptSkip) String() string {
	return fmt.Sprintf("concept DROPPED, so every query, mutation and shape bound to it "+
		"will fail at runtime: %v", s.Err)
}

// LoadUnifiedConcepts loads every concept in the mounted tree and
// registers it. See LoadUnifiedConceptsWithSkips for the variant that
// also reports which concepts were dropped.
func LoadUnifiedConcepts(logger *slog.Logger) (int, error) {
	n, _, err := LoadUnifiedConceptsWithSkips(logger)
	return n, err
}

// LoadUnifiedConceptsWithSkips is LoadUnifiedConcepts plus the list of
// concepts that failed to build. Boot ignores the list (a skip must not
// stop a node starting); memqllint turns each entry into a diagnostic,
// which is the only pre-boot gate a product bundle has.
func LoadUnifiedConceptsWithSkips(logger *slog.Logger) (int, []ConceptSkip, error) {
	var skips []ConceptSkip
	tree := memqldsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return 0, nil, fmt.Errorf("walk unified DSL tree: %w", err)
	}

	// Pass 1: read + parse every concept file, cache its decls + `use`
	// imports, and build a global index dir -> { conceptName -> canonicalId }.
	// The index is what resolves @relationship targets written as
	// `use`-imported bare concept names (#1067): a target's canonical id
	// can carry a sub-namespace (e.g. `state` -> v1:cognition:turn:state),
	// so it must be looked up from the target concept's own assembled id,
	// not derived from the import path.
	type conceptFile struct {
		path  string
		dir   string
		decls []*languageAst.ConceptDecl
		uses  []*languageParser.UseDeclaration
	}
	var files []conceptFile
	index := make(map[string]map[string]string) // dir -> conceptName -> canonicalId

	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			if logger != nil {
				logger.Warn("unified loader: skipping unreadable file",
					"component", "memql.unifiedLoader",
					"file", p,
					"error", openErr)
			}
			continue
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			continue
		}

		decls := ExtractConceptDecls(string(raw))
		if len(decls) == 0 {
			continue
		}
		// Concept files are pure schema today, so `use` imports are new
		// to them (#1067). Best-effort: a parse failure just yields no
		// imports and cross-namespace targets fall through to the warning.
		uses, _ := parsedUseDeclarations(string(raw))
		dir := firstPathSegment(p)
		files = append(files, conceptFile{path: p, dir: dir, decls: decls, uses: uses})

		for _, decl := range decls {
			id, idErr := languageAst.AssembleConceptIdFromDeclInDir(decl, dir, namespacePin(tree, dir))
			if idErr != nil || id == "" {
				continue
			}
			if index[dir] == nil {
				index[dir] = make(map[string]string)
			}
			index[dir][decl.Name] = id
		}
	}

	// Pass 2: resolve relationship targets through the index, then build +
	// register each concept.
	concepts := make(map[string]*memoryNodes.Concept)

	for _, cf := range files {
		for _, decl := range cf.decls {
			id, idErr := languageAst.AssembleConceptIdFromDeclInDir(decl, cf.dir, namespacePin(tree, cf.dir))
			if idErr != nil {
				// The moved-file guard (#2614) and malformed attributes
				// are load ERRORS, not warn-skips: a silently dropped or
				// mis-namespaced concept changes canonical ids, so boot,
				// the memqllint parity tier, and the sense build must all
				// fail loudly here.
				return 0, skips, fmt.Errorf("%s: %w", cf.path, idErr)
			}

			resolveRelationshipTargets(decl, cf.dir, cf.uses, index, id, logger)

			concept, buildErr := memoryNodes.BuildConceptFromDecl(decl, id)
			if buildErr != nil {
				// Recorded as well as logged: this skip drops the WHOLE
				// concept, so every construct bound to it fails at runtime
				// (memql#2909).
				skips = append(skips, ConceptSkip{File: cf.path, Concept: id, Err: buildErr})
				if logger != nil {
					logger.Warn("unified loader: skipping concept that failed to build",
						"component", "memql.unifiedLoader",
						"file", cf.path,
						"concept", id,
						"error", buildErr)
				}
				continue
			}

			concepts[id] = concept
		}
	}

	// Merge into the global registry. Use the additive MergeAll
	// path so legacy registrations stay present during the
	// transitional state.
	memoryNodes.MergeAll(concepts)

	if logger != nil {
		logger.Info("unified loader: registered concepts",
			"component", "memql.unifiedLoader",
			"count", len(concepts))
	}

	return len(concepts), skips, nil
}

// firstPathSegment returns the leading path component of a unified-tree
// path ("cognition/concepts.memql" -> "cognition"; a registered pack
// overlay's "<domain>/concepts.memql" -> "<domain>"). This is the namespace
// directory, which matches the first
// segment of a `use <dir>.concepts.{ ... }` import's dotted path.
func firstPathSegment(p string) string {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

// resolveRelationshipTargets rewrites every @relationship target that is
// written as a `use`-imported bare concept name (#1067) into its canonical
// id, in place on the decl, BEFORE the concept is built. A target already in
// canonical form (contains ':') is left untouched -- the engine resolves both
// forms so the engine and pack trees can migrate independently; the
// authored form is pinned to the bare-name form by each repo's conformance
// test. An unresolved bare name is logged with a migration hint and left as
// written (engine bootstrap then warns that the target concept is unknown).
func resolveRelationshipTargets(
	decl *languageAst.ConceptDecl,
	dir string,
	uses []*languageParser.UseDeclaration,
	index map[string]map[string]string,
	conceptId string,
	logger *slog.Logger,
) {
	for i := range decl.Relationships {
		t := decl.Relationships[i].Target
		if t == "" || strings.Contains(t, ":") {
			continue
		}
		if resolved := resolveRelationshipTargetName(t, dir, uses, index); resolved != "" {
			decl.Relationships[i].Target = resolved
			continue
		}
		if logger != nil {
			logger.Warn("unified loader: unresolved @relationship target -- add a `use <ns>.concepts.{ "+t+" }` import",
				"component", "memql.unifiedLoader",
				"concept", conceptId,
				"target", t)
		}
	}
}

// resolveRelationshipTargetName resolves a bare concept name to its canonical
// id: first against the owning file's own concepts (same-namespace, no import
// needed), then through the file's `use <ns>.concepts.{ name }` imports
// (cross-namespace). Returns "" when neither resolves it.
func resolveRelationshipTargetName(
	name string,
	dir string,
	uses []*languageParser.UseDeclaration,
	index map[string]map[string]string,
) string {
	if m, ok := index[dir]; ok {
		if id, ok := m[name]; ok {
			return id
		}
	}
	for _, u := range uses {
		if u == nil || len(u.Parts) < 2 || u.Parts[1] != "concepts" {
			continue
		}
		imported := false
		for _, n := range u.Names {
			if n == name {
				imported = true
				break
			}
		}
		if !imported {
			continue
		}
		if m, ok := index[u.Parts[0]]; ok {
			if id, ok := m[name]; ok {
				return id
			}
		}
	}
	return ""
}

// Ensure the standard library + memql packages are referenced so go
// vet doesn't complain about unused imports if the loader is
// extended later.
var _ = fmt.Sprintf

// namespacePin reads the domain's one-line namespace.pin file -- the #2614
// escape hatch declaring a DELIBERATE @namespace divergence from the
// directory (the id-preserving I11 move: dsl/deployment pins "cluster").
// Returns "" when no pin exists. The pin file is not a .memql file, so
// every walker ignores it; go:embed all:<domain> and the runtime domain
// mounts both carry it.
func namespacePin(tree fs.FS, dir string) string {
	f, err := tree.Open(dir + "/namespace.pin")
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
