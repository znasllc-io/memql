package dslimports

// graph.go exposes a loaded Tree's symbol declarations and module layout as a
// reusable, read-only Index -- the resolution source the MemQL Sense language
// service consumes for import/reference diagnostics and segment-aware
// completion. It reuses the exact in-package resolution the referential-
// integrity lanes use (buildDeclIndex, resolveUseModule), so the editor agrees
// with what `memqllint` (and, transitively, engine boot) would accept.
//
// Every "does X exist" answer is deliberately framed so a caller can tell
// "proven absent" from "cannot be proven from this workspace": a namespace the
// tree does not own, or a target file that parsed only to its imports
// projection, yields a not-decidable answer rather than a false negative. The
// sense-side adapter maps that onto its inconclusive state so the editor never
// squiggles a legitimate external (engine) import.

import (
	"sort"
	"strings"
)

// Index is a queryable projection of a loaded Tree, built once and safe to
// share. Build it with Tree.NewIndex after Load. Because Load returns a partial
// Tree even on error, an Index is available -- and useful -- even when the
// workspace has broken references, which is exactly when the editor most needs
// reference resolution.
type Index struct {
	tree *Tree
	idx  *declIndex
}

// NewIndex builds the reusable symbol index over an already-loaded Tree.
func (t *Tree) NewIndex() *Index {
	return &Index{tree: t, idx: t.buildDeclIndex()}
}

// HasNamespace reports whether the workspace tree has any file under the
// top-level namespace dir ns. A false answer means the namespace is not in THIS
// workspace -- it may still be a valid external (engine) namespace -- so callers
// treat "not present" as inconclusive, never as "unknown namespace".
func (ix *Index) HasNamespace(ns string) bool {
	return ix.idx.namespaces[ns]
}

// Namespaces returns the workspace's top-level namespace dirs, sorted.
func (ix *Index) Namespaces() []string {
	out := make([]string, 0, len(ix.idx.namespaces))
	for ns := range ix.idx.namespaces {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Kinds returns the module kind segments directly under namespace ns -- the
// filenames (minus .memql) of ns/'s direct children, e.g. {concepts, queries,
// mutations, logic} for a typical product namespace. Sorted.
func (ix *Index) Kinds(ns string) []string {
	seen := make(map[string]bool)
	prefix := ns + "/"
	for path := range ix.tree.Files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		if strings.IndexByte(rest, '/') >= 0 {
			continue // nested; not a direct module file
		}
		seen[strings.TrimSuffix(rest, ".memql")] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ModuleResolves reports whether the dotted module <ns>.<kind> resolves to a
// file in the workspace tree, mirroring resolveUseModule (the Lane-1 rule).
func (ix *Index) ModuleResolves(ns, kind string) bool {
	_, ok := ix.tree.resolveUseModule([]string{ns, kind})
	return ok
}

// ModuleSymbols returns the declared symbol names importable from module
// <ns>.<kind>, sorted. importsOnly is true when the target file parsed only to
// its imports projection (its declarations are not visible), in which case an
// empty list proves nothing. ok is false when the module does not resolve.
func (ix *Index) ModuleSymbols(ns, kind string) (names []string, importsOnly bool, ok bool) {
	target, resolved := ix.tree.resolveUseModule([]string{ns, kind})
	if !resolved {
		return nil, false, false
	}
	if ix.tree.ImportsOnly[target.file] {
		return nil, true, true
	}
	decls := ix.idx.byFile[target.file]
	out := make([]string, 0, len(decls))
	for name := range decls {
		if target.prefix != "" {
			// Consolidated capability files carry dotted names under a prefix;
			// project them back to the importable leaf for this module.
			if !strings.HasPrefix(name, target.prefix+".") {
				continue
			}
			out = append(out, strings.TrimPrefix(name, target.prefix+"."))
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, false, true
}

// SymbolDeclared reports whether id is declared in module <ns>.<kind>. decidable
// is false when the module does not resolve, its declarations are not visible
// (imports-only), or it is an open capability namespace (verbs bound textually
// at boot, never cataloged) -- in every such case the caller stays silent
// rather than flagging a false positive.
func (ix *Index) SymbolDeclared(ns, kind, id string) (declared bool, decidable bool) {
	target, resolved := ix.tree.resolveUseModule([]string{ns, kind})
	if !resolved {
		return false, false
	}
	if ix.tree.ImportsOnly[target.file] {
		return false, false
	}
	if target.prefix != "" && openCapabilityNamespaces[strings.SplitN(target.prefix, ".", 2)[0]] {
		return false, false
	}
	full := id
	if target.prefix != "" {
		full = target.prefix + "." + id
	}
	_, ok := ix.idx.byFile[target.file][full]
	return ok, true
}

// ConceptDeclared reports whether a concept named `name` is declared in the
// workspace tree. nsHint, when non-empty, restricts the match to a concept in
// that namespace (mirroring the boot resolver's namespace hint); an empty hint
// matches by bare name across the tree. decidable is false when nsHint names a
// namespace absent from this workspace (external), so a miss is inconclusive.
func (ix *Index) ConceptDeclared(name, nsHint string) (declared bool, decidable bool) {
	if nsHint != "" && !ix.idx.namespaces[nsHint] {
		return false, false
	}
	entries, ok := ix.idx.concepts[name]
	if !ok {
		return false, true
	}
	if nsHint == "" {
		return true, true
	}
	for _, e := range entries {
		if i := strings.IndexByte(e.file, '/'); i > 0 && e.file[:i] == nsHint {
			return true, true
		}
	}
	return false, true
}
