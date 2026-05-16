// memql-arch walks a Go workspace and emits a topology.model.json
// describing its architecture: cluster, services, packages, types,
// and the relationships between them. The output is consumed by
// memQL Cockpit to render the Topology tab and by anything else that
// wants to talk about the system in software-architecture terms
// (docs exporters, observability overlays, CI gates on import
// cycles, etc.).
//
// Usage:
//
//	memql-arch                                # run in $PWD, write topology.model.json
//	memql-arch --root /path/to/workspace      # explicit root
//	memql-arch --out custom.json              # alternate output path
//	memql-arch --cluster "local"              # name the synthetic L1 root
//	memql-arch --types                        # also include the L4 class graph
//
// Exit codes: 0 success, 1 hard failure (workspace not found, etc.).
// Per-package errors are printed as warnings and do not change the
// exit code -- a partial model is more useful than no model.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/visionarys-io/memql/component/architecture/extract"
	"github.com/visionarys-io/memql/component/architecture/model"
)

func main() {
	var (
		root      = flag.String("root", ".", "workspace root (directory containing go.work or go.mod)")
		out       = flag.String("out", "", "output path (default: <root>/"+model.CanonicalFilename+")")
		cluster   = flag.String("cluster", "", "cluster name (default: workspace folder name)")
		withType  = flag.Bool("types", true, "include the L4 type pass (structs, interfaces, methods)")
		withCalls = flag.Bool("calls", false, "include the CHA call-graph pass (requires --types)")
	)
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fail("resolve root: %v", err)
	}
	m, err := extract.Run(extract.Options{
		WorkspaceRoot:    absRoot,
		ClusterName:      *cluster,
		IncludeTypes:     *withType,
		IncludeCallGraph: *withCalls,
	})
	if err != nil {
		fail("%v", err)
	}

	outPath := *out
	if outPath == "" {
		outPath = filepath.Join(absRoot, model.CanonicalFilename)
	}
	if err := m.WriteFile(outPath); err != nil {
		fail("write model: %v", err)
	}

	fmt.Printf("memql-arch: wrote %d nodes, %d edges -> %s\n", len(m.Nodes), len(m.Edges), outPath)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "memql-arch: "+format+"\n", args...)
	os.Exit(1)
}
