// Static guard: every `//go:generate` directive names inputs that exist
// (znasllc-io/memql#3215).
//
// # The failure mode
//
// `component/server/doc.go` carried two directives whose every referenced path
// was absent from the tree -- `component/schemas/open_api.yaml`,
// `scripts/postprocess_server_gen/main.go`, and the `server.gen.go` they
// claimed to produce. `make generate` had been broken for some time and nobody
// ran it, because there is no `go generate` step in CI.
//
// The cost was not the broken target. It was that the directives asserted
// `component/server` is generated from an OpenAPI spec, sitting in the
// package's own `doc.go`, where a reader trusts it. The package is
// hand-written.
//
// Nothing verified the claim, so nothing could report it had stopped being
// true. This guard is what makes the next one fail loudly instead.
//
// # Why it refuses rather than guesses
//
// Deciding which tokens of an arbitrary command line are paths is not
// decidable in general: `-o server.gen.go` names an output that must NOT
// exist yet, `github.com/oapi-codegen/...@v2.5.1` looks like a path and is a
// module, and `sh -c "cd ../../.. && make x"` is an opaque shell payload.
//
// A guard that guesses wrong in the permissive direction reports a broken
// directive as clean, which is the defect it exists to catch. So this
// implements a deliberately small set of directive shapes and FAILS on
// anything outside it, asking the author to extend the guard. That is the
// same fail-closed posture as CompilePattern in scripts/ci/pathsfilter.go,
// for the same reason.
package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generateDirective is one `//go:generate` line and where it was found.
type generateDirective struct {
	file string // repo-relative, slash-separated
	dir  string // absolute directory the directive resolves paths against
	line int
	text string // everything after the `//go:generate ` prefix
}

// collectGenerateDirectives finds every `//go:generate` line in the tree.
func collectGenerateDirectives(t *testing.T, root string) []generateDirective {
	t.Helper()

	var out []generateDirective
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// Must be column 0 to be a directive; an indented one is a comment.
			if !strings.HasPrefix(line, "//go:generate ") {
				continue
			}
			out = append(out, generateDirective{
				file: filepath.ToSlash(rel),
				dir:  filepath.Dir(path),
				line: i + 1,
				text: strings.TrimSpace(strings.TrimPrefix(line, "//go:generate ")),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	return out
}

// splitArgs tokenizes on whitespace, keeping a double-quoted run as one token.
// Enough for the shapes below; anything needing more is refused upstream.
func splitArgs(s string) []string {
	var (
		args []string
		cur  strings.Builder
		inQ  bool
	)
	flush := func() {
		if cur.Len() > 0 {
			args = append(args, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
		case (r == ' ' || r == '\t') && !inQ:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return args
}

// isRemoteModule reports whether a `go run` target is a module path rather
// than a path on disk: `example.com/x@v1` or `example.com/x`.
//
// The leading-relative check comes first and is not merely a shortcut. Go
// spells a local target `./x` or `../x`, and those first segments contain a
// dot -- so the dotted-first-segment heuristic alone classifies every relative
// path as remote and silently checks nothing. That is exactly the permissive
// misfire this guard must not make.
func isRemoteModule(arg string) bool {
	if strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") || strings.HasPrefix(arg, "/") {
		return false
	}
	if strings.Contains(arg, "@") {
		return true
	}
	// A module path's first segment is a domain, which must contain a dot.
	first, _, _ := strings.Cut(arg, "/")
	return strings.Contains(first, ".")
}

// requiredInputs returns the paths a directive requires to already exist,
// relative to the directive's own directory. The bool is false when the
// directive's shape is not one this guard understands.
func requiredInputs(args []string) (paths []string, understood bool) {
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "sh", "bash":
		// `sh -c "<script>"` is an opaque payload: its contents are a shell
		// program, not a path list, and this guard does not parse shell.
		// Accepted with no claims, but only in exactly that form.
		return nil, len(args) >= 3 && args[1] == "-c"

	case "protoc":
		for _, a := range args[1:] {
			switch {
			case strings.HasPrefix(a, "--proto_path="):
				paths = append(paths, strings.TrimPrefix(a, "--proto_path="))
			case strings.HasSuffix(a, ".proto"):
				paths = append(paths, a)
			case strings.HasPrefix(a, "-"):
				// Output dirs (`--go_out=gen`) and codegen options
				// (`--go_opt=paths=source_relative`) make no input claim.
			default:
				return nil, false // an unrecognised positional
			}
		}
		return paths, true

	case "go":
		if len(args) < 3 || args[1] != "run" {
			return nil, false
		}
		if !isRemoteModule(args[2]) {
			paths = append(paths, args[2])
		}
		for _, a := range args[3:] {
			// A bare filename is ambiguous -- `-o server.gen.go` names an
			// output. Only a token carrying a separator is read as an input
			// path, which is the conservative direction.
			if !strings.HasPrefix(a, "-") && strings.Contains(a, "/") {
				paths = append(paths, a)
			}
		}
		return paths, true
	}
	return nil, false
}

// TestGoGenerateDirectivesReferenceExistingPaths is the guard.
func TestGoGenerateDirectivesReferenceExistingPaths(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	directives := collectGenerateDirectives(t, root)
	if len(directives) == 0 {
		t.Fatal("found no //go:generate directives anywhere -- either they were " +
			"all removed or this guard's discovery is broken. Failing closed " +
			"rather than reporting a vacuous pass")
	}

	for _, d := range directives {
		args := splitArgs(d.text)
		inputs, understood := requiredInputs(args)
		if !understood {
			t.Errorf("%s:%d: this guard does not understand the directive %q.\n"+
				"It refuses unfamiliar shapes rather than guessing which tokens "+
				"are paths, because guessing permissively would report a broken "+
				"directive as clean. Extend requiredInputs in %s to describe this "+
				"command, then this directive is checked like the others.",
				d.file, d.line, d.text, "go_generate_directives_test.go")
			continue
		}
		for _, p := range inputs {
			if _, err := os.Stat(filepath.Join(d.dir, filepath.FromSlash(p))); err != nil {
				t.Errorf("%s:%d: directive references %q, which does not exist "+
					"(resolved against %s).\n"+
					"`go generate ./...` fails on this, and there is no go generate "+
					"step in CI -- so the directive also asserts something false "+
					"about how this package is maintained, in the package's own "+
					"source, with nothing to report that it stopped being true.",
					d.file, d.line, p, filepath.ToSlash(filepath.Dir(d.file)))
			}
		}
	}
}
