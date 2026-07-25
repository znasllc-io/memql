package memql

import (
	"io/fs"

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
type senseWorkspaceGraph struct {
	idx *dslimports.Index
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
			File:   s.File,
			Name:   s.Name,
			Kind:   s.Kind,
			Line:   s.Line,
			Column: s.Column,
		})
	}
	return out
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
	tree, _ := dslimports.Load(root)
	if tree == nil {
		return nil
	}
	return senseWorkspaceGraph{idx: tree.NewIndex()}
}
