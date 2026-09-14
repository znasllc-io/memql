// Command attributematrix writes the attribute matrix page
// (docs/public/language/attribute-matrix.md) from the annotation registry in
// component/language/annotations and, with -check, fails when the committed
// page is stale (memql#5360).
//
// The page is generated: every annotation, where each construct and kind of
// field accepts it, its argument forms, keys, examples and docs, and every
// retirement come from the registry the parser checks annotations against.
// Change the registry, then regenerate; a hand edit of the page is refused by
// TestAttributeMatrixIsGenerated in the root package.
//
//	make docs-matrix          write the page
//	make docs-matrix-check    CI gate: the page must equal what the registry renders
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/znasllc-io/memql/component/language/dslspec"
)

func main() {
	var (
		out   = flag.String("out", dslspec.AttributeMatrixPath, "the page to write, or with -check to compare")
		check = flag.Bool("check", false, "compare the page with what the registry renders; write nothing")
	)
	flag.Parse()
	msg, err := run(*out, *check)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attributematrix: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(msg)
}

// run renders the page and writes it to path, or with check compares it with
// what is there. It returns the line to print on success.
func run(path string, check bool) (string, error) {
	want := dslspec.AttributeMatrix()
	got, err := os.ReadFile(path)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return "", err
	}
	if check {
		if missing {
			return "", fmt.Errorf("%s does not exist.\n\nRun `make docs-matrix` and commit the result", path)
		}
		if string(got) != want {
			return "", fmt.Errorf("%s is stale against the annotation registry.\n\n"+
				"Run `make docs-matrix` and commit the result; the page is generated, so change the registry "+
				"(component/language/annotations), never the page", path)
		}
		return "attribute matrix current: " + path, nil
	}
	if !missing && string(got) == want {
		return "attribute matrix unchanged: " + path, nil
	}
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		return "", err
	}
	return "wrote " + path, nil
}
