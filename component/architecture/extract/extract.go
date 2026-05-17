// Package extract reads a Go workspace (or a single module) and
// produces a model.Model describing its architecture. It is the
// "compiler frontend" of the architecture framework -- the model is
// the IR, and renderers (cockpit TUI, exporters) are the backends.
//
// The package is split by concern:
//   - workspace.go : discovering modules from go.work / go.mod
//   - archyaml.go  : per-module declarations
//   - gomod.go     : module path lookup
//   - packages.go  : L2 (services) + L3 (packages, imports)
//   - cluster.go   : synthetic L1 cluster root
//   - types.go     : L4 (types, methods, embedding, implements) -- follow-on milestone
//
// Run is the public entry point used by both cmd/memql-arch and any
// test harness. Callers that want to merge in deployment metadata or
// observability overlays should compose around Run rather than
// editing it -- the model is append-only by design.
package extract

import (
	"fmt"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// Options controls one run of the extractor.
type Options struct {
	// WorkspaceRoot is the directory containing go.work (or, for
	// single-module runs, go.mod). Defaults to the current directory
	// at the CLI level.
	WorkspaceRoot string

	// ClusterName labels the synthetic L1 root. Defaults to the base
	// name of WorkspaceRoot.
	ClusterName string

	// IncludeTypes enables the L4 extractor (types + methods +
	// embedding + implements). Off by default because the type
	// pass is meaningfully more expensive on large workspaces; the
	// caller turns it on when they want class diagrams.
	IncludeTypes bool

	// IncludeCallGraph enables the CHA-based call-graph pass. Adds
	// EdgeCalls between Method/Func nodes. Requires IncludeTypes
	// to be on as well (the call-graph edges only make sense when
	// their endpoints exist in the model). Costly on large
	// workspaces -- the SSA build dominates -- so off by default.
	IncludeCallGraph bool
}

// Run executes the extractor and returns a fully populated model.
// Errors are returned at the first hard failure (workspace not found,
// unparseable go.work, packages.Load fatal); per-package problems are
// logged to stdout but do not abort the run.
func Run(opts Options) (*model.Model, error) {
	if opts.WorkspaceRoot == "" {
		return nil, fmt.Errorf("WorkspaceRoot is required")
	}
	ws, err := DiscoverWorkspace(opts.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	plans, err := Plan(ws)
	if err != nil {
		return nil, err
	}

	m := model.NewModel(ws.Root)

	if err := ExtractPackages(m, plans, ws.Root); err != nil {
		return nil, err
	}

	if opts.IncludeTypes {
		if err := ExtractTypes(m, plans, ws.Root); err != nil {
			return nil, err
		}
	}

	if opts.IncludeCallGraph {
		if !opts.IncludeTypes {
			return nil, fmt.Errorf("IncludeCallGraph requires IncludeTypes")
		}
		if err := ExtractCalls(m, plans); err != nil {
			return nil, err
		}
	}

	clusterName := opts.ClusterName
	if clusterName == "" {
		clusterName = defaultClusterName(ws.Root)
	}
	ExtractCluster(m, clusterName)

	return m, nil
}

// defaultClusterName takes the base name of the workspace root. Kept
// in a helper because filepath.Base does the right thing on every
// platform we care about and the policy belongs in one place.
func defaultClusterName(root string) string {
	// Imported lazily to keep this file's import list aligned with
	// what's actually used; filepath is in stdlib.
	return baseName(root)
}
