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
//
// The workflow is parsed with yaml.v3 rather than scanned line by line, the way
// scripts/citags/tags_test.go does it. That is not a stylistic preference: an
// adversarial review of the first version of this guard defeated it twice over
// by exploiting line-scanning. Putting the correct pattern in the step's `name:`
// while leaving `run:` narrow satisfied a scan that harvested patterns from the
// whole line -- reinstating the exact bug this file exists to prevent, green.
// A trailing `# was ./cmd/memql-lsp/...` comment did the same. Reading `Run`
// from a parsed step makes both unrepresentable rather than merely tested for.
package dev

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// lspCmdDir is the tree whose packages the vscode-extension lane must cover.
const lspCmdDir = "cmd/memql-lsp"

// vscodeLaneJob is the job key this guard asserts on.
const vscodeLaneJob = "vscode-extension"

type ciWorkflow struct {
	Jobs map[string]struct {
		If    string `yaml:"if"`
		Steps []struct {
			Name            string `yaml:"name"`
			Run             string `yaml:"run"`
			If              string `yaml:"if"`
			ContinueOnError any    `yaml:"continue-on-error"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

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

func ciYAML(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read .github/workflows/ci.yml: %v", err)
	}
	return raw
}

// vscodeLane returns the parsed vscode-extension job.
func vscodeLane(t *testing.T) struct {
	If    string `yaml:"if"`
	Steps []struct {
		Name            string `yaml:"name"`
		Run             string `yaml:"run"`
		If              string `yaml:"if"`
		ContinueOnError any    `yaml:"continue-on-error"`
	} `yaml:"steps"`
} {
	t.Helper()
	var wf ciWorkflow
	if err := yaml.Unmarshal(ciYAML(t), &wf); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	job, ok := wf.Jobs[vscodeLaneJob]
	if !ok {
		t.Fatalf("no %q job in .github/workflows/ci.yml; if the lane was renamed, "+
			"retarget this guard rather than deleting it (#2792)", vscodeLaneJob)
	}
	return job
}

// goTestSteps returns the lane's steps whose `run:` invokes `go test`.
//
// Only the parsed `Run` scalar is ever considered. A pattern in the step's
// `name:`, or in a `#` comment inside a block scalar, is not a command and
// cannot satisfy any assertion here.
func goTestSteps(t *testing.T) []struct {
	Name            string `yaml:"name"`
	Run             string `yaml:"run"`
	If              string `yaml:"if"`
	ContinueOnError any    `yaml:"continue-on-error"`
} {
	t.Helper()
	var out []struct {
		Name            string `yaml:"name"`
		Run             string `yaml:"run"`
		If              string `yaml:"if"`
		ContinueOnError any    `yaml:"continue-on-error"`
	}
	for _, s := range vscodeLane(t).Steps {
		for _, line := range commandLines(s.Run) {
			if strings.Contains(line, "go test") {
				out = append(out, s)
				break
			}
		}
	}
	return out
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

// pkgPatternRe matches a Go package pattern argument: a relative path, with or
// without a `/...` suffix. Quotes are stripped before matching, so a correctly
// quoted pattern is not a false alarm -- quoting is reformatting, and this file
// asserts on meaning.
var pkgPatternRe = regexp.MustCompile(`(^|\s)(\./\S*|\.\.\.)`)

// runSelectorRe matches the flags that narrow which TESTS run, as opposed to
// which packages. `-skip` is included because it is the natural way to silence
// a specific failing drift guard, and `--` / `-test.` spellings because Go's
// flag package accepts them identically.
var runSelectorRe = regexp.MustCompile(`(^|[\s=])--?(test\.)?(run|skip)[\s=]`)

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

// The lane must run the tests of EVERY package under cmd/memql-lsp, not just
// one subtree. This is the assertion that fails against the pre-#2792 recipe.
func TestVSCodeLaneCoversEveryLSPPackage(t *testing.T) {
	steps := goTestSteps(t)
	if len(steps) == 0 {
		t.Fatal("the vscode-extension lane runs no `go test` at all; the " +
			"editors/vscode drift guards in cmd/memql-lsp would never run on an " +
			"editors/vscode-only PR, because go-checks path-skips it (#2792)")
	}

	var patterns []string
	for _, s := range steps {
		for _, line := range commandLines(s.Run) {
			if !strings.Contains(line, "go test") {
				continue
			}
			for _, m := range pkgPatternRe.FindAllStringSubmatch(strings.ReplaceAll(strings.ReplaceAll(line, `"`, " "), `'`, " "), -1) {
				patterns = append(patterns, m[2])
			}
		}
	}
	if len(patterns) == 0 {
		t.Fatal("no package pattern found in the lane's `go test` invocation(s); " +
			"a bare `go test` tests only the current directory (#2792)")
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

// A `-run` or `-skip` selector would narrow the scope WITHIN the packages,
// which the coverage assertion above cannot see. This repo has been bitten by
// exactly that: `make arch-model-check` ran `-run` against a test that did not
// exist and reported success having run nothing (#2923, #3003).
//
// GOFLAGS is checked too: `GOFLAGS=-run=TestNothing go test ./...` applies the
// selector without the flag ever appearing as an argument.
func TestVSCodeLaneTestStepSelectsNoSubsetOfTests(t *testing.T) {
	for _, s := range goTestSteps(t) {
		for _, line := range commandLines(s.Run) {
			if !strings.Contains(line, "go test") && !strings.Contains(line, "GOFLAGS") {
				continue
			}
			if runSelectorRe.MatchString(line) {
				t.Errorf("the lane's `go test` must not narrow which tests run "+
					"(-run/-skip, including via GOFLAGS): it is invisible to the "+
					"package-coverage check, and a -run pattern matching nothing "+
					"exits 0 reporting success (#2923).\ngot: %s", line)
			}
		}
	}
}

// A gate whose failure is swallowed is not a gate.
//
// `continue-on-error` is a step KEY, never part of the command, so it is read
// from the parsed step rather than from the command text. The first version of
// this guard looked for it in the command line, where it can never appear --
// an adversarial review defeated the gate by setting it as a sibling key while
// this test stayed green.
func TestVSCodeLaneTestStepDoesNotSuppressFailure(t *testing.T) {
	for _, s := range goTestSteps(t) {
		if s.ContinueOnError != nil && s.ContinueOnError != false {
			t.Errorf("the lane's `go test` step sets continue-on-error=%v; its "+
				"failure would not fail the lane (#2792).\nstep: %s", s.ContinueOnError, s.Name)
		}
		// Read pipefail from the COMMANDS, not the raw payload: a `# pipefail is
		// handled by the runner` comment must not satisfy it. Checking a string
		// against the wrong surface is the defect this file has now made three
		// times (continue-on-error against command text, patterns against whole
		// lines); the commandLines pass is the surface that means "shell ran it".
		pipefail := false
		for _, line := range commandLines(s.Run) {
			if strings.Contains(line, "pipefail") {
				pipefail = true
				break
			}
		}
		for _, line := range commandLines(s.Run) {
			for _, esc := range []string{"|| true", "|| :", "|| exit 0"} {
				if strings.Contains(line, esc) {
					t.Errorf("the lane's `go test` must not suppress its failure exit (%q).\ngot: %s", esc, line)
				}
			}
			// A pipeline reports the LAST command's status. GitHub runs `bash -e`
			// without `pipefail`, so `go test ... | tee out.txt` exits with tee's
			// status and a failing drift guard goes green -- the same swallowed
			// failure as `|| true`, wearing different punctuation.
			if strings.Contains(line, "go test") && !pipefail && hasPipe(line) {
				t.Errorf("the lane's `go test` is piped, and the step does not set "+
					"`pipefail`; the step would exit with the LAST command's status, "+
					"so a failing drift guard reports green (#2792).\ngot: %s", line)
			}
		}
	}
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

// Coverage of the tests is worthless if the STEP never executes. A step-level
// `if:` that evaluates false leaves every assertion above green while the lane
// runs nothing -- the same silent under-coverage one level up.
func TestVSCodeLaneTestStepIsUnconditional(t *testing.T) {
	for _, s := range goTestSteps(t) {
		if strings.TrimSpace(s.If) != "" {
			t.Errorf("the lane's `go test` step is gated on `if: %s`. The step must "+
				"run whenever the lane does; a false condition reports green having "+
				"run nothing (#2792).\nstep: %s", s.If, s.Name)
		}
	}
}

// ...and worthless if the JOB never runs on the PRs that need it. This is the
// rung above the step-level check, and the realistic one: dropping
// `needs.changes.outputs.vscode` from the job's `if:` leaves a condition that
// still reads plausibly (`ci` plus the non-PR events) while an
// editors/vscode-only PR -- every dependabot bump to the extension -- stops
// selecting the lane at all. Every other assertion in this file stays green,
// and #2792 is restored end to end.
//
// Like TestVSCodeBucketStillCoversTheExtensionTree this READS the condition
// without owning its shape: any `if:` that still consults the vscode bucket
// passes, however it is otherwise rewritten.
func TestVSCodeLaneRunsOnExtensionOnlyChanges(t *testing.T) {
	cond := vscodeLane(t).If
	if strings.TrimSpace(cond) == "" {
		return // unconditional: the lane always runs, which is stronger still
	}
	if !strings.Contains(cond, "needs.changes.outputs.vscode") {
		t.Errorf("the vscode-extension job's `if:` no longer consults the `vscode` "+
			"path bucket, so an editors/vscode-only PR -- the shape every dependabot "+
			"bump takes -- would not select this lane. go-checks skips that PR too, "+
			"so the drift guards would run nowhere (#2792).\ngot if: %s", cond)
	}
}

// And coverage is worthless if the LANE never runs on the PRs that need it.
// Narrowing the `vscode` path filter to, say, `editors/vscode/src/**` makes a
// package.json-only dependabot bump path-skip the lane entirely -- restoring
// #2792's failure end to end, with every assertion above still green.
//
// This reads the filter; it does not own it. Widening the bucket is fine and
// keeps this green; only dropping `editors/vscode` coverage fails.
func TestVSCodeBucketStillCoversTheExtensionTree(t *testing.T) {
	var doc struct {
		Jobs map[string]struct {
			Outputs map[string]string `yaml:"outputs"`
			Steps   []struct {
				Id   string `yaml:"id"`
				With struct {
					Filters string `yaml:"filters"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ciYAML(t), &doc); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}

	// The lane consults `needs.changes.outputs.vscode`, which the `changes` job
	// wires from ONE step's outputs. Find that step by the id the output
	// expression names, rather than taking the last step that happens to carry
	// filters: with two paths-filter steps present, "last wins" reads the wrong
	// block in both directions -- a decoy step can mask a narrowed real one, and
	// an unrelated second filter reddens a lane that is perfectly correct.
	wiring := doc.Jobs["changes"].Outputs["vscode"]
	m := regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.outputs`).FindStringSubmatch(wiring)
	if m == nil {
		t.Fatalf("cannot tell which step supplies the `changes` job's `vscode` "+
			"output (got %q); this guard cannot pass vacuously (#2792)", wiring)
	}
	stepID := m[1]

	var filters string
	var found bool
	for _, s := range doc.Jobs["changes"].Steps {
		if s.Id == stepID {
			filters, found = s.With.Filters, true
			break
		}
	}
	if !found {
		t.Fatalf("the `changes` job's `vscode` output names step %q, which has no "+
			"matching step in the job (#2792)", stepID)
	}
	if filters == "" {
		t.Fatalf("step %q on the `changes` job declares no path filters; this "+
			"guard cannot pass vacuously (#2792)", stepID)
	}

	var parsed map[string][]string
	if err := yaml.Unmarshal([]byte(filters), &parsed); err != nil {
		t.Fatalf("parse the `changes` filters block: %v", err)
	}
	want := "editors/vscode/**"
	for _, p := range parsed["vscode"] {
		if p == want {
			return
		}
	}
	t.Errorf("the `vscode` bucket no longer contains %q (got %v). A PR touching "+
		"only editors/vscode/package.json would path-skip the vscode-extension "+
		"lane, and go-checks skips it too -- so the drift guards would not run "+
		"anywhere, which is #2792 restored end to end.", want, parsed["vscode"])
}
