package memql

import (
	"io/fs"
	"path"
	"strings"

	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// workspace_graph.go adapts a dslimports.Index (the file/tree resolution
// memqllint uses) to the sense.WorkspaceGraph the language service consumes for
// import/reference diagnostics and segment-aware completion. It translates the
// index's decidable/not-decidable facts into sense's tri-state Resolved,
// treating anything the workspace cannot prove -- a namespace not mounted here,
// an imports-only target -- as ResolvedUnknown so the editor never false-flags a
// legitimate external (engine) import.

// senseWorkspaceGraph wraps a dslimports.Index as a sense.WorkspaceGraph.
//
// prefix is the path from the root the CALLER passed down to the DSL root the
// index was actually built from ("" when they are the same directory). The
// index keys namespaces off the first path segment, so it must be rooted where
// the domain directories live; but every path this type hands back is relative
// to the caller's root, so the prefix is restored on the way out.
type senseWorkspaceGraph struct {
	idx    *dslimports.Index
	prefix string
}

func (g senseWorkspaceGraph) ModuleResolves(ns, kind string) sense.Resolved {
	if !g.idx.HasNamespace(ns) {
		return sense.ResolvedUnknown // external namespace -- not ours to judge
	}
	if g.idx.ModuleResolves(ns, kind) {
		return sense.ResolvedYes
	}
	return sense.ResolvedNo
}

func (g senseWorkspaceGraph) SymbolDeclared(ns, kind, id string) sense.Resolved {
	if !g.idx.HasNamespace(ns) {
		return sense.ResolvedUnknown
	}
	declared, decidable := g.idx.SymbolDeclared(ns, kind, id)
	if !decidable {
		return sense.ResolvedUnknown
	}
	if declared {
		return sense.ResolvedYes
	}
	return sense.ResolvedNo
}

func (g senseWorkspaceGraph) ConceptExists(name, nsHint string) sense.Resolved {
	declared, decidable := g.idx.ConceptDeclared(name, nsHint)
	if !decidable {
		return sense.ResolvedUnknown
	}
	if declared {
		return sense.ResolvedYes
	}
	return sense.ResolvedNo
}

func (g senseWorkspaceGraph) HasNamespace(ns string) bool { return g.idx.HasNamespace(ns) }
func (g senseWorkspaceGraph) HasImportsOnlyFiles() bool   { return g.idx.HasImportsOnlyFiles() }

func (g senseWorkspaceGraph) Namespaces() []string     { return g.idx.Namespaces() }
func (g senseWorkspaceGraph) Kinds(ns string) []string { return g.idx.Kinds(ns) }

func (g senseWorkspaceGraph) SymbolsInModule(ns, kind string) []string {
	names, _, _ := g.idx.ModuleSymbols(ns, kind)
	return names
}

func (g senseWorkspaceGraph) DeclarationSites(name string) []sense.DeclSite {
	sites := g.idx.DeclarationSites(name)
	if len(sites) == 0 {
		return nil
	}
	out := make([]sense.DeclSite, 0, len(sites))
	for _, s := range sites {
		out = append(out, sense.DeclSite{
			File:   path.Join(g.prefix, s.File),
			Name:   s.Name,
			Kind:   s.Kind,
			Line:   s.Line,
			Column: s.Column,
		})
	}
	return out
}

// dslRootCandidates are the conventional places a DSL tree sits relative to an
// opened workspace. The engine keeps its domains under dsl/; a product bundle
// repo may keep them at the top level, which resolveDSLRoot handles by testing
// the root itself first.
var dslRootCandidates = []string{"dsl"}

// resolveDSLRoot returns the sub-tree the workspace graph should resolve
// against, plus the path from root down to it.
//
// This is what makes reference resolution work in a real editor session. An
// author opens the REPOSITORY in VS Code, so root is the repo -- but dslimports
// derives a namespace from the first path segment, which from there is
// uniformly "dsl". Every namespace query (HasNamespace / Kinds /
// SymbolsInModule) then answers for a namespace called "dsl" that no `use` line
// ever names, so segment-aware completion offered nothing and import
// diagnostics could never prove a symbol missing (memql#2762). Rooting the
// index where the domain directories actually live is what makes
// `use calendar.concepts.{ ... }` resolvable.
func resolveDSLRoot(root fs.FS) (fs.FS, string) {
	if root == nil {
		return nil, ""
	}
	if isDSLRoot(root) {
		return root, ""
	}
	for _, candidate := range dslRootCandidates {
		sub, err := fs.Sub(root, candidate)
		if err != nil {
			continue
		}
		if isDSLRoot(sub) {
			return sub, candidate
		}
	}
	return root, ""
}

// isDSLRoot reports whether d's immediate children look like DSL domain
// directories: at least two of them directly contain a .memql file.
//
// Two rather than one deliberately. A repository root whose dsl/ directory
// happens to hold a stray .memql would otherwise look like a domain holder and
// win over the real tree below it; requiring a second sibling domain makes the
// answer stable against scratch files.
func isDSLRoot(d fs.FS) bool {
	entries, err := fs.ReadDir(d, ".")
	if err != nil {
		return false
	}
	domains := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		if !directoryHasDirectMemqlFile(d, name) {
			continue
		}
		domains++
		if domains >= 2 {
			return true
		}
	}
	return false
}

// directoryHasDirectMemqlFile reports whether dir holds a .memql file
// DIRECTLY (not in a nested sub-directory), which is what makes it look like a
// DSL domain rather than a container of them.
func directoryHasDirectMemqlFile(root fs.FS, dir string) bool {
	entries, err := fs.ReadDir(root, dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".memql") {
			return true
		}
	}
	return false
}

// buildWorkspaceGraph loads the workspace tree and returns a sense.WorkspaceGraph
// over it. It returns nil (-> the service's no-op graph) only when there is no
// workspace to resolve against (a nil root). A partial Load -- broken
// references, a malformed file -- still yields a usable graph, because
// dslimports.Load returns a partial Tree on error. That is the whole point:
// reference resolution must survive exactly the broken workspace it is meant to
// diagnose, so the Load error is intentionally ignored here (the engine build
// surfaces boot errors separately).
func buildWorkspaceGraph(root fs.FS) sense.WorkspaceGraph {
	if root == nil {
		return nil
	}
	dslRoot, prefix := resolveDSLRoot(root)
	tree, _ := dslimports.Load(dslRoot)
	if tree == nil {
		return nil
	}
	return senseWorkspaceGraph{idx: tree.NewIndex(), prefix: prefix}
}
