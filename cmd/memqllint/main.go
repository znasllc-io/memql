// memqllint runs the same DSL-load + diagnostic pipeline the engine
// runs at startup, on demand against a .memql file or a whole DSL
// root. Sibling tool to memqlfmt and memqlmigrate.
//
// Usage:
//
//	memqllint [flags] [path]
//
//	go run ./cmd/memqllint dsl/
//	go run ./cmd/memqllint --json dsl/cognition/logic.memql
//
// Flags:
//
//	--json, -j   Emit machine-readable JSON output.
//	--help, -h   Print usage.
//
// Path defaults to the current working directory. A single .memql
// file scopes the diagnostic report to that file + its transitively-
// imported neighbors; pointing at a directory loads the whole tree
// rooted there.
//
// Exit codes:
//
//	0  success (no diagnostics)
//	1  diagnostics found
//	2  invalid usage / filesystem error
//
// History: this command used to live in memql-cockpit
// (`memql-cockpit lint`) and shelled the same dslimports.Load
// pipeline that lives here. memql#278 moved the surface into memql
// so the dsl-lint CI gate doesn't need a cross-repo cockpit install.
// memql-cockpit's `lint` subcommand stays as a thin alias for
// authoring ergonomics; it now imports this package's logic instead
// of carrying its own copy (or will, once cockpit's parallel PR
// catches up).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql/dslimports"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	var (
		jsonOut bool
		path    string
	)

	// Tiny argv parser: --json flag in any position; first positional
	// is the path. Avoids pulling flag.NewFlagSet in for two args.
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json", a == "-j":
			jsonOut = true
		case a == "--help", a == "-h":
			printUsage()
			return 0
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "ERROR: unknown flag %q\n", a)
			printUsage()
			return 2
		default:
			if path != "" {
				fmt.Fprintf(os.Stderr, "ERROR: too many positional arguments (got %q after %q)\n", a, path)
				return 2
			}
			path = a
		}
	}

	if path == "" {
		// Default to cwd.
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: getting working directory: %v\n", err)
			return 2
		}
		path = cwd
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: resolving path %q: %v\n", path, err)
		return 2
	}

	info, err := os.Stat(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 2
	}

	var rootDir, target string
	if info.IsDir() {
		rootDir = abs
		target = ""
	} else if strings.HasSuffix(abs, ".memql") {
		// Single-file mode: the DSL root is the file's directory; we
		// load the whole directory but emit diagnostics scoped to the
		// target file + its transitively-imported neighbors.
		rootDir = filepath.Dir(abs)
		rel, relErr := filepath.Rel(rootDir, abs)
		if relErr != nil {
			fmt.Fprintf(os.Stderr, "ERROR: resolving relative target: %v\n", relErr)
			return 2
		}
		target = filepath.ToSlash(rel)
	} else {
		fmt.Fprintf(os.Stderr, "ERROR: target %q is neither a directory nor a .memql file\n", abs)
		return 2
	}

	root := os.DirFS(rootDir)
	tree, loadErr := dslimports.Load(root)

	report := buildReport(tree, loadErr, target)
	report.Root = rootDir

	if jsonOut {
		return emitJSON(report)
	}
	return emitHuman(report, rootDir)
}

// Report is the structured view of a lint run, suitable for JSON
// output or human-rendering.
type Report struct {
	Root   string       `json:"root"`
	Files  int          `json:"files"`
	Order  []string     `json:"order,omitempty"`
	Errors []Diagnostic `json:"errors,omitempty"`
}

// Diagnostic carries one error from the load pipeline. Level is
// always "error" today; warning + info levels land when the
// downstream validators wire in.
type Diagnostic struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

func buildReport(tree *dslimports.Tree, loadErr error, target string) *Report {
	r := &Report{}
	if tree != nil {
		r.Files = len(tree.Files)
		// Only emit the order when there were no errors -- with
		// errors the topo step may have skipped nodes and the list
		// is misleading.
		if loadErr == nil {
			r.Order = tree.Order
		}
	}
	if loadErr != nil {
		for _, d := range flattenDiagnostics(loadErr) {
			if target != "" && !errorMentionsFile(d, target) {
				// Single-file mode: skip diagnostics about other
				// files in the tree.
				continue
			}
			r.Errors = append(r.Errors, Diagnostic{
				Level:   "error",
				Message: d.Error(),
			})
		}
		sort.SliceStable(r.Errors, func(i, j int) bool {
			return r.Errors[i].Message < r.Errors[j].Message
		})
	}
	return r
}

// flattenDiagnostics walks any chain of multi-error wrappers
// (LoadError, BuildErrors, ...) and returns the leaf errors as a
// flat slice so the user sees one diagnostic line per actual
// problem, not the wrapper's header line repeated.
func flattenDiagnostics(err error) []error {
	if err == nil {
		return nil
	}
	type multiUnwrap interface{ Unwrap() []error }
	if mu, ok := err.(multiUnwrap); ok {
		var out []error
		for _, sub := range mu.Unwrap() {
			out = append(out, flattenDiagnostics(sub)...)
		}
		return out
	}
	return []error{err}
}

// errorMentionsFile returns true if the diagnostic's text mentions
// the file (or anything in its directory). Used by single-file mode
// to filter the report. Currently a string-contains check; structured
// per-file diagnostics will get exact filtering once the validator
// emits them with explicit file fields.
func errorMentionsFile(d error, target string) bool {
	return strings.Contains(d.Error(), target)
}

func emitJSON(r *Report) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: encoding JSON: %v\n", err)
		return 2
	}
	if len(r.Errors) > 0 {
		return 1
	}
	return 0
}

func emitHuman(r *Report, rootDir string) int {
	if len(r.Errors) == 0 {
		fmt.Printf("OK: %d file(s) loaded, no diagnostics.\n", r.Files)
		return 0
	}
	fmt.Printf("ERROR: %d diagnostic(s) in %s\n", len(r.Errors), rootDir)
	for _, d := range r.Errors {
		fmt.Printf("  - %s\n", d.Message)
	}
	return 1
}

func printUsage() {
	fmt.Println("Usage: memqllint [flags] [path]")
	fmt.Println("")
	fmt.Println("Validates a .memql file or directory using the dslimports.Load")
	fmt.Println("pipeline. Path defaults to the current working directory.")
	fmt.Println("")
	fmt.Println("Flags:")
	fmt.Println("  --json, -j   Emit machine-readable JSON output")
	fmt.Println("  --help, -h   Show this help")
	fmt.Println("")
	fmt.Println("Exit codes:")
	fmt.Println("  0  success (no diagnostics)")
	fmt.Println("  1  diagnostics found")
	fmt.Println("  2  invalid usage / filesystem error")
}
