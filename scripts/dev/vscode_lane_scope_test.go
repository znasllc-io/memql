// Static guard over the vscode-extension lane's test scope
// (znasllc-io/memql#2792).
//
// Background: every drift guard over a checked-in `editors/vscode` asset lives
// in the ROOT `cmd/memql-lsp` package -- `languageconfig_test.go` guards
// `language-configuration.json`, `extensionmanifest_test.go` guards
// `package.json`'s `engines.vscode` and `engines.node` floors. The lane used to
// run `go test ./cmd/memql-lsp/internal/grammar/...`, which is a SIBLING
// package, so none of them were run by it.
//
// The other lane that would run them, `go-checks`, gates on the `go` bucket
// (`**/*.go`, `go.mod`, `go.sum`, `go.work*`). A PR touching only
// `editors/vscode/package.json` + `package-lock.json` matches the `vscode`
// bucket and NOT `go`, so `go-checks` path-skips and neither guard fires --
// which is the exact shape every dependabot bump to the extension takes, and
// the exact case those guards were written for. Each of them ran on its own PR
// (each added a `.go` file, so `go` matched) and would then have gone quiet
// forever.
//
// This guard exists so the lane cannot silently narrow again.
//
// WHY IT LIVES HERE AND NOT IN cmd/memql-lsp: a guard in the root lsp package
// would be run by the lane it asserts on. Narrowing the lane back to
// `internal/grammar` would stop the guard running, so it would go silent at
// precisely the moment it should fire. It has to sit in a package reached
// independently -- `scripts/dev` is covered by `go test ./...` and matches the
// `go` bucket, so it runs whether or not the vscode lane does.
//
// It follows the principle stated in this package's gitleaks_scan_scope_test.go
// (#2996, the same defect class -- a CI lane whose scope silently under-covers):
// assert on MEANING rather than spelling, so reformatting the step is fine and
// only a real narrowing fails.
package dev

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// lspCmdDir is the tree whose packages the vscode-extension lane must cover.
const lspCmdDir = "cmd/memql-lsp"

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

// vscodeLaneBody returns the `vscode-extension:` job block from ci.yml.
//
// The block runs from the job key to the next key at the same indentation,
// which is how a lane is delimited in this workflow. Taking the whole file
// instead would let a `go test` in an unrelated lane satisfy the assertions.
func vscodeLaneBody(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read .github/workflows/ci.yml: %v", err)
	}
	lines := strings.Split(string(raw), "\n")

	jobKey := regexp.MustCompile(`^  [A-Za-z0-9_-]+:\s*$`)
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "vscode-extension:" && jobKey.MatchString(line) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("no `vscode-extension:` job found in .github/workflows/ci.yml; " +
			"if the lane was renamed, retarget this guard rather than deleting it (#2792)")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if jobKey.MatchString(lines[i]) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// goTestLines returns the lane's `go test` invocations, comments excluded so no
// assertion can be satisfied by prose.
func goTestLines(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(vscodeLaneBody(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "go test") {
			continue
		}
		found = append(found, trimmed)
	}
	return found
}

// pkgPatternRe matches a Go package pattern argument: a relative path, with or
// without a `/...` suffix. Flag values are excluded by requiring a leading `.`.
var pkgPatternRe = regexp.MustCompile(`(^|\s)(\./\S*|\.\.\.)`)

// coveredBy reports whether the package directory `dir` (repo-relative, slash
// separated) is matched by the `go test` package pattern `pat`.
//
// This mirrors `go help packages`: a trailing `...` matches the prefix and
// everything beneath it, and any other pattern matches exactly one directory.
func coveredBy(dir, pat string) bool {
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
func testBearingLSPPackages(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var dirs []string
	err := filepath.Walk(filepath.Join(root, lspCmdDir), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, "_test.go") {
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

// The lane must run the tests of EVERY package under cmd/memql-lsp, not just
// one subtree. This is the assertion that fails against the pre-#2792 recipe.
func TestVSCodeLaneCoversEveryLSPPackage(t *testing.T) {
	cmds := goTestLines(t)
	if len(cmds) == 0 {
		t.Fatal("the vscode-extension lane runs no `go test` at all; the " +
			"editors/vscode drift guards in cmd/memql-lsp would never run on an " +
			"editors/vscode-only PR, because go-checks path-skips it (#2792)")
	}

	var patterns []string
	for _, cmd := range cmds {
		for _, m := range pkgPatternRe.FindAllStringSubmatch(cmd, -1) {
			patterns = append(patterns, m[2])
		}
	}
	if len(patterns) == 0 {
		t.Fatalf("no package pattern found in the lane's `go test` invocation(s); "+
			"a bare `go test` tests only the current directory.\ngot: %v", cmds)
	}

	for _, dir := range testBearingLSPPackages(t) {
		covered := false
		for _, pat := range patterns {
			if coveredBy(dir, pat) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("package %s declares tests but is not covered by the "+
				"vscode-extension lane's patterns %v.\n"+
				"Every drift guard over a checked-in editors/vscode asset lives in "+
				"%s, and go-checks path-skips an editors/vscode-only PR -- the shape "+
				"every dependabot bump takes -- so this lane is the ONLY place those "+
				"guards get to fire (#2792).", dir, patterns, lspCmdDir)
		}
	}
}

// A `-run` selector would narrow the scope back WITHIN the packages, which the
// coverage assertion above cannot see. This repo has been bitten by exactly
// that: `make arch-model-check` ran `-run` against a test that did not exist
// and reported success having run nothing (#2923, #3003).
func TestVSCodeLaneTestStepSelectsNoSubsetOfTests(t *testing.T) {
	for _, cmd := range goTestLines(t) {
		if regexp.MustCompile(`(^|\s)--?(test\.)?run[\s=]`).MatchString(cmd) {
			t.Errorf("the lane's `go test` must not pass -run: it narrows the scope "+
				"within the packages, invisibly to the coverage check, and a pattern "+
				"matching nothing exits 0 reporting success (#2923).\ngot: %s", cmd)
		}
	}
}

// A gate whose failure is swallowed is not a gate.
func TestVSCodeLaneTestStepDoesNotSuppressFailure(t *testing.T) {
	for _, cmd := range goTestLines(t) {
		for _, esc := range []string{"|| true", "|| :", "continue-on-error"} {
			if strings.Contains(cmd, esc) {
				t.Errorf("the lane's `go test` must not suppress its failure exit (%q).\ngot: %s", esc, cmd)
			}
		}
	}
}
