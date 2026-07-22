package memql

// Tests for the sense.WorkspaceGraph adapter (workspace_graph.go): the tri-state
// mapping that lets the editor resolve workspace-local references while staying
// silent on anything it cannot prove (external namespaces, partial loads), and
// the BuildOfflineSense wiring that keeps the graph alive even when engine boot
// fails.

import (
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/memql/sense"
)

func fyloWorkspace() fstest.MapFS {
	return fstest.MapFS{
		"fylo/concepts.memql": &fstest.MapFile{Data: []byte(`@version("1.0.0")
@namespace("fylo")
concept order {
  id  string  @required
}`)},
		"fylo/queries.memql": &fstest.MapFile{Data: []byte(`use fylo.concepts.{ order }

@enabled
query order listOrders {
  args { id  string  @required }
  filter  id == args.id
}`)},
	}
}

func TestSenseWorkspaceGraph_TriState(t *testing.T) {
	g := buildWorkspaceGraph(fyloWorkspace())
	if g == nil {
		t.Fatal("buildWorkspaceGraph returned nil for a real workspace")
	}

	// Workspace-owned namespace: definite yes/no.
	if got := g.ModuleResolves("fylo", "concepts"); got != sense.ResolvedYes {
		t.Errorf("ModuleResolves(fylo, concepts) = %v, want ResolvedYes", got)
	}
	if got := g.ModuleResolves("fylo", "concept"); got != sense.ResolvedNo {
		t.Errorf("ModuleResolves(fylo, concept) = %v, want ResolvedNo (wrong kind)", got)
	}
	if got := g.SymbolDeclared("fylo", "concepts", "order"); got != sense.ResolvedYes {
		t.Errorf("SymbolDeclared(fylo, concepts, order) = %v, want ResolvedYes", got)
	}
	if got := g.SymbolDeclared("fylo", "concepts", "oder"); got != sense.ResolvedNo {
		t.Errorf("SymbolDeclared(fylo, concepts, oder) = %v, want ResolvedNo", got)
	}
	if got := g.ConceptExists("order", ""); got != sense.ResolvedYes {
		t.Errorf("ConceptExists(order) = %v, want ResolvedYes", got)
	}
	if got := g.ConceptExists("full", ""); got != sense.ResolvedNo {
		t.Errorf("ConceptExists(full) = %v, want ResolvedNo", got)
	}

	// External (unmounted) namespace: inconclusive, never a false negative --
	// this is what stops fylo's `use platform.mutations.{...}` from squiggling.
	if got := g.ModuleResolves("platform", "mutations"); got != sense.ResolvedUnknown {
		t.Errorf("ModuleResolves(platform, mutations) = %v, want ResolvedUnknown", got)
	}
	if got := g.SymbolDeclared("platform", "mutations", "stageOutboundRequest"); got != sense.ResolvedUnknown {
		t.Errorf("SymbolDeclared(platform, ...) = %v, want ResolvedUnknown", got)
	}
	if got := g.ConceptExists("user", "identity"); got != sense.ResolvedUnknown {
		t.Errorf("ConceptExists(user, identity) = %v, want ResolvedUnknown", got)
	}

	// Enumerations for completion.
	if got := g.Namespaces(); len(got) != 1 || got[0] != "fylo" {
		t.Errorf("Namespaces() = %v, want [fylo]", got)
	}
	if got := g.SymbolsInModule("fylo", "concepts"); len(got) != 1 || got[0] != "order" {
		t.Errorf("SymbolsInModule(fylo, concepts) = %v, want [order]", got)
	}
}

func TestBuildWorkspaceGraph_NilRoot(t *testing.T) {
	if g := buildWorkspaceGraph(nil); g != nil {
		t.Errorf("buildWorkspaceGraph(nil) = %v, want nil (-> service no-op graph)", g)
	}
}

// A workspace with a broken import still yields a working graph: dslimports.Load
// returns a partial tree on error, so reference resolution survives exactly the
// broken workspace it is meant to diagnose.
func TestBuildWorkspaceGraph_SurvivesBrokenReference(t *testing.T) {
	ws := fyloWorkspace()
	ws["fylo/more.memql"] = &fstest.MapFile{Data: []byte(`use fylo.nonesuch.{ ghost }
`)}
	g := buildWorkspaceGraph(ws)
	if g == nil {
		t.Fatal("buildWorkspaceGraph returned nil despite a partial (broken-ref) load")
	}
	if got := g.ModuleResolves("fylo", "concepts"); got != sense.ResolvedYes {
		t.Errorf("after a broken ref, ModuleResolves(fylo, concepts) = %v, want ResolvedYes", got)
	}
}

// The go-blind fix: when a workspace construct trips the strict-boot gate,
// BuildOfflineSense still returns a service carrying the workspace graph, so
// reference resolution keeps working on exactly the broken workspace -- where
// the old code fell back to a graph-less New(nil).
func TestBuildOfflineSense_GraphSurvivesInitFailure(t *testing.T) {
	root := fstest.MapFS{
		"demo/concepts.memql": &fstest.MapFile{Data: []byte(`@version("1.0.0")
@namespace("demo")
concept item {
  id  string  @required
}`)},
		"demo/mutations.memql": &fstest.MapFile{Data: []byte(`use demo.concepts.{ item }

mutate ghostConcept touchGhost {
  args { id  string!  }
  update {
    id: args.id
  }
}`)},
	}

	svc, err := BuildOfflineSense(root)
	if err == nil {
		t.Fatal("expected a strict-boot error for a mutation bound to a nonexistent concept")
	}
	if svc == nil {
		t.Fatal("BuildOfflineSense returned a nil service on init failure -- the workspace graph was lost")
	}
	if got := svc.Workspace().ModuleResolves("demo", "concepts"); got != sense.ResolvedYes {
		t.Errorf("graph lost on init failure: ModuleResolves(demo, concepts) = %v, want ResolvedYes", got)
	}
}
