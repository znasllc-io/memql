// The editor's filesystem guards must run on editor-only and dependency changes.
package dev

import (
	"github.com/znasllc-io/memql/core/repowalk"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const lspCmdDir = "cmd/memql-lsp"

var pkgPatternRe = regexp.MustCompile(`(^|\s)(\./\S*|\.\.\.)`)
var runSelectorRe = regexp.MustCompile(`(^|[\s=])--?(test\.)?(run|skip)[\s=]`)

// repoRoot returns the repository root, resolved from this file's own location
// rather than the working directory, so the test is runnable from anywhere.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/dev/ -> repo root is two directories up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// commandLines splits a `run:` payload into shell command lines, dropping
// comments so no assertion can be satisfied -- or defeated -- by prose.
//
// A `#` only begins a comment at the start of a word and outside quotes, which
// is what the shell does. Truncating at the first `#` unconditionally reddens a
// correct lane whose command contains one in a string (`echo "### drift" &&
// go test ./cmd/memql-lsp/...` parsed as no command at all).
func commandLines(run string) []string {
	var out []string
	for _, line := range strings.Split(run, "\n") {
		out = append(out, stripComment(line))
	}
	var kept []string
	for _, line := range out {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}

// stripComment removes a trailing shell comment, respecting quoting.
func stripComment(line string) string {
	var quote rune
	for i, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// coveredBy reports whether the package directory `dir` (repo-relative, slash
// separated) is matched by the `go test` package pattern `pat`.
//
// This mirrors `go help packages`: a trailing `...` matches the prefix and
// everything beneath it, and any other pattern matches exactly one directory.
// The `prefix+"/"` form matters -- a plain string prefix would let the pattern
// `./cmd/memql-ls/...` claim to cover `cmd/memql-lsp`.
func coveredBy(dir, pat string) bool {
	pat = strings.Trim(pat, `"'`)
	pat = strings.TrimPrefix(pat, "./")
	pat = strings.TrimSuffix(pat, "/")
	if pat == "..." {
		return true // ./... -- the whole module
	}
	if prefix, ok := strings.CutSuffix(pat, "/..."); ok {
		return dir == prefix || strings.HasPrefix(dir, prefix+"/")
	}
	return dir == pat
}

// testBearingLSPPackages enumerates every directory under cmd/memql-lsp that
// declares Go tests.
//
// Enumerated rather than hardcoded on purpose: a package added under
// cmd/memql-lsp later is automatically required to be covered, without anyone
// remembering to update this list. That is the property that makes the guard
// outlive the change it was written for.
//
// `testdata` and `_`/`.`-prefixed directories are skipped because the Go
// toolchain ignores them: `go test ./x/...` never selects a package there, so
// requiring the lane to cover one would be an assertion the toolchain cannot
// satisfy.
func testBearingLSPPackages(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var dirs []string
	err := filepath.Walk(filepath.Join(root, lspCmdDir), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if repowalk.SkipDir(info.Name()) {
				return filepath.SkipDir
			}
			base := filepath.Base(path)
			if base == "testdata" || strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, seen := range dirs {
			if seen == rel {
				return nil
			}
		}
		dirs = append(dirs, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", lspCmdDir, err)
	}
	// Anti-vacuous floor: a walk that finds nothing must fail rather than pass
	// over an empty set. Without this, a broken walk or a moved tree reports
	// "every package is covered" having checked none.
	if len(dirs) == 0 {
		t.Fatalf("found no test-bearing packages under %s; this guard cannot "+
			"pass vacuously (#2792)", lspCmdDir)
	}
	return dirs
}

// hasPipe reports whether the line contains a shell pipe, ignoring `||` and
// any `|` inside quotes.
//
// The quote tracking mirrors stripComment: a `|` in `-args "a|b"` is data, not
// a pipeline, and flagging it would red a correct lane.
func hasPipe(line string) bool {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '|':
			if i+1 < len(line) && line[i+1] == '|' {
				i++ // `||` is a logical OR, handled separately above
				continue
			}
			if i > 0 && line[i-1] == '|' {
				continue
			}
			return true
		}
	}
	return false
}
func TestVSCodeLaneCoversEveryLSPPackage(t *testing.T) {
	patterns := nativeSuite(t, "editor-packages")
	for _, dir := range testBearingLSPPackages(t) {
		covered := false
		for _, pat := range patterns {
			if coveredBy(dir, pat) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("editor checks omit LSP guard package %s: %v", dir, patterns)
		}
	}
}
func TestVSCodeLaneRunsOnExtensionAndDependencyChanges(t *testing.T) {
	_, step := nativeStep(t, "editor-packages")
	nativeStepCoversPaths(t, step, "editors/vscode/package.json", "editors/vscode/package-lock.json", "editors/vscode/src/extension.ts", "cmd/memql-lsp/main.go", "sdk/ts/package.json", "sdk/ts-viewkit/package.json")
}
