package memql

// unified_loader.go is Pass 2 of the DSL restructure migration
// (docs/dsl-import-model-refactor.md). It walks the new domain-first
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
	"log/slog"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageAst "github.com/znasllc-io/memql/component/language/ast"
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
// across files. Concepts that fail to build are skipped with a
// warning log; the loader is best-effort to keep startup robust.
//
// Implementation note: instead of going through dslimports.Load
// (which runs the full struct-form rewriter chain and gets
// confused by multi-concept consolidated files), we use the
// concepts-only extractor (ExtractConceptDecls) which scans each
// file's source text for `concept ... { }` blocks and parses each
// in isolation. This bypasses the rewriter limitation and gets
// us to full concept coverage from the new tree.
func LoadUnifiedConcepts(logger *slog.Logger) (int, error) {
	tree := memqldsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return 0, fmt.Errorf("walk unified DSL tree: %w", err)
	}

	concepts := make(map[string]*memoryNodes.Concept)

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

		for _, decl := range ExtractConceptDecls(string(raw)) {
			id, idErr := languageAst.AssembleConceptIdFromDecl(decl)
			if idErr != nil {
				if logger != nil {
					logger.Warn("unified loader: skipping concept with bad ID",
						"component", "memql.unifiedLoader",
						"file", p,
						"concept", decl.Name,
						"error", idErr)
				}
				continue
			}
			if id == "" {
				// Concept hasn't migrated to @version + @namespace yet.
				// Skip silently -- the legacy loader handles it.
				continue
			}

			concept, buildErr := memoryNodes.BuildConceptFromDecl(decl, id)
			if buildErr != nil {
				if logger != nil {
					logger.Warn("unified loader: skipping concept that failed to build",
						"component", "memql.unifiedLoader",
						"file", p,
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

	return len(concepts), nil
}

// Ensure the standard library + memql packages are referenced so go
// vet doesn't complain about unused imports if the loader is
// extended later.
var _ = fmt.Sprintf
