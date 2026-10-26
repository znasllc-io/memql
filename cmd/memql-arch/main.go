// memql-arch generates a source topology for inspection and build verification.
// make arch-model owns the canonical flag set and writes an ignored output.
// The topology is not embedded in engine images or consumed by the live OS.
// Any package/dependency/type-load error fails before output is written.
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
		canonical       = flag.Bool("canonical", false, "require the repository toolchain and fixed complete structural profile")
		root            = flag.String("root", ".", "workspace root (directory containing go.work or go.mod)")
		out             = flag.String("out", "", "output path (default: <root>/"+model.CanonicalFilename+")")
		cluster         = flag.String("cluster", "", "cluster name (default: workspace folder name)")
		withAutomations = flag.Bool("automations", false, "include the DSL automation trigger graph")
		withType        = flag.Bool("types", true, "include the L4 type pass (structs, interfaces, methods)")
		withCalls       = flag.Bool("calls", false, "include the CHA call-graph pass (requires --types)")
		repro           = flag.Bool("reproducible", false, "blank generated_at and workspace so the output depends only on the code (used by `make arch-model`)")
	)
	flag.Parse()

	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fail("resolve root: %v", err)
	}
	if *canonical {
		if !*withType || !*withAutomations || !*repro || *cluster != "memql" {
			fail("canonical generation requires types, automations, reproducible output and cluster memql")
		}
		if err := prepareCanonical(absRoot); err != nil {
			fail("canonical environment: %v", err)
		}
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

	if *withAutomations {
		if err := addAutomations(m); err != nil {
			fail("%v", err)
		}
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
	if err := writeGeneratedModel(m, outPath); err != nil {
		fail("write model: %v", err)
	}

	fmt.Printf("memql-arch: complete package loading (types=%t, calls=%t, automations=%t)\n", *withType, *withCalls, *withAutomations)
	fmt.Printf("memql-arch: wrote %d nodes, %d edges -> %s\n", len(m.Nodes), len(m.Edges), outPath)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "memql-arch: "+format+"\n", args...)
	os.Exit(1)
}

// The ignored output directory does not exist in a clean checkout.
func writeGeneratedModel(m *model.Model, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return m.WriteFile(path)
}
