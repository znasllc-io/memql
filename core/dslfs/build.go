package dslfs

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// RawImport is the parser's view of one entry in an `import (...)`
// block: the literal path string from source plus the explicit
// alias (empty when omitted). The build pipeline resolves the path
// to a root-relative canonical form and derives the alias from the
// basename if Alias is empty.
//
// Caller responsibility: feed RawImport slices into BuildImportGraph
// keyed by the importing file's root-relative path. dslfs has no
// parser dependency; the real loader parses .memql files and
// translates ast.ImportDecl into RawImport at the call site.
type RawImport struct {
	Path  string // raw source: "./cognition/participant", "../foo"
	Alias string // explicit alias from `as <name>`, empty otherwise
}

// FileImports is the per-file resolved-and-aliased import table.
// Keys are the alias the importing file binds (default-derived from
// basename or explicit `as`). Values are root-relative canonical
// paths to the imported file.
type FileImports map[string]string

// BuildImportGraph resolves every file's RawImports into canonical
// paths + validated aliases, populates an ImportGraph, and returns
// the per-file alias tables. Errors are accumulated across files
// so the validator can surface every problem in one pass.
//
//   - rawByFile keys are the root-relative paths of importing files
//     (the same shape WalkMemqlFiles emits). Values are the parsed
//     imports for each file.
//
// Resolution per import:
//
//  1. ResolveImport(file, entry.Path) -> canonical path or error.
//  2. Alias = entry.Alias if non-empty, else DefaultAlias(canonical).
//  3. ValidateAlias(alias).
//  4. Reject duplicate aliases within the same file's import block.
//  5. Reject unused-import / missing-target checks happen later in
//     the per-file symbol-resolution pass; this layer only cares
//     about graph + alias integrity.
//
// Returned graph nodes include every importing file (always) and
// every imported file (added by edges). The caller should also
// call graph.AddNode for every file the walker discovered so leaves
// with no imports + no importers still get registered.
func BuildImportGraph(rawByFile map[string][]RawImport) (*ImportGraph, map[string]FileImports, error) {
	graph := NewImportGraph()
	aliases := make(map[string]FileImports, len(rawByFile))

	// Sort keys for deterministic error ordering.
	files := make([]string, 0, len(rawByFile))
	for f := range rawByFile {
		files = append(files, f)
	}
	sort.Strings(files)

	var errs []error

	for _, importingFile := range files {
		graph.AddNode(importingFile)
		fileAliases := make(FileImports)

		for _, entry := range rawByFile[importingFile] {
			resolved, err := ResolveImport(importingFile, entry.Path)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", importingFile, err))
				continue
			}

			alias := entry.Alias
			if alias == "" {
				alias, err = DefaultAlias(resolved)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: import %q: %w", importingFile, entry.Path, err))
					continue
				}
			} else if err := ValidateAlias(alias); err != nil {
				errs = append(errs, fmt.Errorf("%s: import %q: %w", importingFile, entry.Path, err))
				continue
			}

			if existing, dup := fileAliases[alias]; dup {
				errs = append(errs, fmt.Errorf("%s: alias %q collides: %q vs %q (add `as <name>` to disambiguate)",
					importingFile, alias, existing, resolved))
				continue
			}
			fileAliases[alias] = resolved

			graph.AddEdge(importingFile, resolved)
		}

		aliases[importingFile] = fileAliases
	}

	if len(errs) > 0 {
		return graph, aliases, &BuildErrors{Errors: errs}
	}
	return graph, aliases, nil
}

// BuildErrors aggregates the per-file errors produced during
// BuildImportGraph. The validator pipeline unwraps it and presents
// every error to the author at once.
type BuildErrors struct {
	Errors []error
}

func (b *BuildErrors) Error() string {
	if len(b.Errors) == 0 {
		return "no errors"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d import error(s):\n", len(b.Errors)))
	for _, e := range b.Errors {
		sb.WriteString("  - ")
		sb.WriteString(e.Error())
		sb.WriteString("\n")
	}
	return sb.String()
}

// Unwrap exposes the underlying errors so callers can use
// errors.As / errors.Is on individual entries.
func (b *BuildErrors) Unwrap() []error {
	return b.Errors
}

// HasErrors reports whether err contains any *BuildErrors with
// at least one entry. Convenience for the validator CLI.
func HasErrors(err error) bool {
	if err == nil {
		return false
	}
	var be *BuildErrors
	if errors.As(err, &be) {
		return len(be.Errors) > 0
	}
	return true
}
