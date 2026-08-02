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
//
// Query scope: the module queries take a TWO-segment path (namespace + kind,
// e.g. "fylo"+"concepts" -> fylo/concepts.memql), which covers every product /
// engine module. Multi-segment consolidated-capability paths
// (`capabilities.integration.github` -> the consolidated
// capabilities/capabilities.memql with a dotted construct prefix, which
// resolveUseModule handles) are NOT representable through the (ns, kind) shape;
// a consumer that must resolve those handles them separately. Callers also pass
// an UNVERSIONED namespace -- a leading version segment (`v1.cognition.concepts`)
// is the caller's to strip (cf. stripVersionPrefix). An unstripped "v1"
// namespace is simply absent from the tree, so every query degrades to the
// inconclusive answer, never a false negative.
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

// HasImportsOnlyFiles reports whether the tree contains any file that parsed
// only to its imports projection (its declarations are invisible). When true, a
// "does not exist anywhere" conclusion is unsafe -- the symbol may be declared
// in such a file -- so callers must stay inconclusive.
func (ix *Index) HasImportsOnlyFiles() bool {
	return len(ix.tree.ImportsOnly) > 0
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
// file in the workspace tree, mirroring resolveUseModules (the Lane-1 rule).
func (ix *Index) ModuleResolves(ns, kind string) bool {
	_, ok := ix.tree.resolveUseModules(ix.idx, []string{ns, kind})
	return ok
}

// ModuleSymbols returns the declared symbol names importable from module
// <ns>.<kind>, sorted. importsOnly is true when any target file parsed only to
// its imports projection (its declarations are not visible), in which case an
// empty list proves nothing. ok is false when the module does not resolve.
//
// A namespace can be served by more than one directory -- its own, plus any
// domain pinned to it (memql#2945) -- so the returned set is the union, which
// is what the editor must offer: every one of those names is importable under
// this path and binds at boot.
func (ix *Index) ModuleSymbols(ns, kind string) (names []string, importsOnly bool, ok bool) {
	targets, resolved := ix.tree.resolveUseModules(ix.idx, []string{ns, kind})
	if !resolved {
		return nil, false, false
	}
	seen := make(map[string]bool)
	var out []string
	for _, target := range targets {
		if ix.tree.ImportsOnly[target.file] {
			return nil, true, true
		}
		for name := range ix.idx.byFile[target.file] {
			leaf := name
			if target.prefix != "" {
				// Consolidated capability files carry dotted names under a prefix;
				// project them back to the importable leaf for this module.
				if !strings.HasPrefix(name, target.prefix+".") {
					continue
				}
				leaf = strings.TrimPrefix(name, target.prefix+".")
			}
			if seen[leaf] {
				continue
			}
			seen[leaf] = true
			out = append(out, leaf)
		}
	}
	if out == nil {
		out = []string{}
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
	targets, resolved := ix.tree.resolveUseModules(ix.idx, []string{ns, kind})
	if !resolved {
		return false, false
	}
	for _, target := range targets {
		if ix.tree.ImportsOnly[target.file] {
			return false, false
		}
		if target.prefix != "" && openCapabilityNamespaces[strings.SplitN(target.prefix, ".", 2)[0]] {
			return false, false
		}
	}
	return declaredInAny(ix.idx, targets, id), true
}

// ConceptDeclared reports whether a concept named `name` is declared in the
// workspace tree, following the boot resolver's resolution SHAPE: a concept is
// resolved by UNIQUE trailing-segment (bare) name, and nsHint is only a
// TIEBREAKER among two-or-more concepts sharing the name -- NOT a hard namespace
// filter. So a name that is unique in the tree resolves regardless of the hint
// (matching function_loader.go's resolveConceptByTrailingSegment, where the hint
// only disambiguates duplicates).
//
// decidable is false only when the answer cannot be proven from this workspace:
// the hinted namespace is external (absent here), so the concept may live there.
// A bare name absent from the tree is reported (false, true) at this level; the
// consumer layers file-context conservatism on top -- whether the importing file
// pulls in an external namespace that could hold it (cf.
// dslimports.verifySignatureBindings / missingIsProvable) -- since this
// tree-global query has no importing-file context.
func (ix *Index) ConceptDeclared(name, nsHint string) (declared bool, decidable bool) {
	entries, ok := ix.idx.concepts[name]
	if !ok {
		if nsHint != "" && !ix.idx.namespaces[nsHint] {
			return false, false // hint is external -- the concept may live there
		}
		return false, true
	}
	if len(entries) == 1 || nsHint == "" {
		// A unique bare name resolves regardless of the hint; an ambiguous name
		// with no hint still exists (ambiguity is the consumer's concern, not an
		// existence question).
		return true, true
	}
	if !ix.idx.namespaces[nsHint] {
		return false, false // ambiguous, and the hint is external -- undecidable
	}
	for _, e := range entries {
		if i := strings.IndexByte(e.file, '/'); i > 0 && e.file[:i] == nsHint {
			return true, true
		}
	}
	return false, true
}
