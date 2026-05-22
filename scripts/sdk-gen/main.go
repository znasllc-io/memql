// sdk-gen is the thin CLI wrapper over the importable generator package
// (sdk/gen). It walks one or more DSL trees and emits typed Go (and
// optionally TypeScript) methods on `sdk/go/client.QueryClient` for
// every query / mutation / logic declared in `**/*.memql`.
//
// Output (Go):
//
//	sdk/go/client/generated_queries.go
//	sdk/go/client/generated_mutations.go
//	sdk/go/client/generated_logics.go
//
// Multiple DSL roots compose: pass --dsl repeatedly or as a
// comma-separated list. memQL's own `make sdk-gen` passes a single
// root (the core DSL). A product BFF composes `core ∪ its own DSL` --
// typically by importing sdk/gen directly rather than shelling out to
// this CLI, since the BFF's DSL lives in its own repo.
//
// Re-run via `make sdk-gen`. CI gate via `make sdk-gen-check`
// (regenerates + diffs).
//
// All parse + emit logic lives in sdk/gen so a BFF (a separate Go
// module that depends on github.com/znasllc-io/memql) can call it
// directly. This file only handles flag parsing + reporting.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/znasllc-io/memql/sdk/gen"
)

// rootsFlag collects --dsl values: repeatable and/or comma-separated.
type rootsFlag []string

func (r *rootsFlag) String() string { return strings.Join(*r, ",") }

func (r *rootsFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*r = append(*r, part)
		}
	}
	return nil
}

func main() {
	var roots rootsFlag
	flag.Var(&roots, "dsl", "DSL tree root; repeatable or comma-separated to compose multiple roots (default \"dsl\")")
	outDir := flag.String("out", "sdk/go/client", "directory for generated Go files; empty to skip Go emission")
	tsOutDir := flag.String("ts-out", "sdk/ts/src/client", "directory for generated TypeScript files; empty to skip TS emission")
	check := flag.Bool("check", false, "exit non-zero if regenerated files would differ")
	flag.Parse()

	if len(roots) == 0 {
		roots = rootsFlag{"dsl"}
	}

	res, err := gen.Generate(gen.Options{
		Roots: roots,
		GoOut: *outDir,
		TSOut: *tsOutDir,
		Check: *check,
	})
	if err != nil {
		if *check && res != nil && len(res.Drift) > 0 {
			for _, p := range res.Drift {
				fmt.Fprintf(os.Stderr, "drift: %s\n", p)
			}
			fmt.Fprintf(os.Stderr, "\nGenerated SDK is out of date. Run `make sdk-gen` and commit the result.\n")
			os.Exit(1)
		}
		fail(err)
	}

	if *check {
		fmt.Println("sdk-gen: no drift")
		return
	}
	for _, p := range res.Wrote {
		fmt.Printf("wrote %s\n", p)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "sdk-gen: %v\n", err)
	os.Exit(1)
}
