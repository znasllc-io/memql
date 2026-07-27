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
//	memql-arch --calls                        # also include the CHA call graph
//	memql-arch --reproducible                 # omit generated_at + workspace
//
// The checked-in component/architecture/embedded/topology.model.json is
// produced by `make arch-model`, which pins the flag set. Do not regenerate it
// by hand: the flags are load-bearing (the artifact includes the call graph,
// which the defaults do not) and `make arch-model-check` compares against
// exactly that command.
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

	"github.com/znasllc-io/memql/component/architecture/extract"
	"github.com/znasllc-io/memql/component/architecture/model"
)

func main() {
	var (
		root      = flag.String("root", ".", "workspace root (directory containing go.work or go.mod)")
		out       = flag.String("out", "", "output path (default: <root>/"+model.CanonicalFilename+")")
		cluster   = flag.String("cluster", "", "cluster name (default: workspace folder name)")
		withType  = flag.Bool("types", true, "include the L4 type pass (structs, interfaces, methods)")
		withCalls = flag.Bool("calls", false, "include the CHA call-graph pass (requires --types)")
		repro     = flag.Bool("reproducible", false, "blank generated_at and workspace so the output depends only on the code (used by `make arch-model`)")
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

	// --reproducible strips the two fields that make the output depend on WHO
	// ran it and WHEN (memql#2844).
	//
	// The checked-in artifact carried `"workspace": "/Users/znas/..."` -- an
	// absolute path from a different machine, on a worktree that no longer
	// exists -- and a wall-clock `generated_at`. Both change on every run, so a
	// byte-for-byte drift gate was impossible no matter which flags were used,
	// which is why nobody could tell how the file had been produced.
	//
	// Workspace has ZERO in-repo consumers; its own doc says the cockpit uses
	// it "only for display". Blanking it also stops a developer's home
	// directory being committed.
	if *repro {
		m.GeneratedAt = ""
		m.Workspace = ""
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
