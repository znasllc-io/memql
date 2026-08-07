// Static guard over the `gates` path bucket and the lane it selects
// (znasllc-io/memql#2972).
//
// # The defect
//
// Every lane `ci-required` aggregates is gated on a bucket from the `changes`
// job, and the aggregator treats `skipped` as a pass -- deliberately, and
// load-bearing for docs-only PRs. The seven original buckets (`ci`, `go`,
// `dsl`, `sdkts`, `voice`, `proto`, `vscode`) matched no non-Go gate INPUT:
// measured at the time, 383 of 2936 tracked files matched no filter at all.
//
// So a PR touching only `.md`, a manifest, the architecture model or a shell
// script set all seven outputs false, every lane skipped, and `ci-required`
// reported success over a diff nothing verified -- while the Go test that
// would have caught it sat in the skipped step. Confirmed on real history:
// PR #2854 (docs-only) had all seven lanes `skipped` and `ci-required` green,
// and 18 of the last 400 non-merge commits had a whole diff in that set.
//
// # What this guard is for
//
// The `gates` bucket is only worth having if it keeps pointing at files a Go
// test actually reads, and if the lane it selects keeps running the packages
// those tests live in. Both halves can be narrowed later by someone with a
// perfectly good local reason, and neither narrowing turns anything red on its
// own -- which is how the original hole was open long enough to be measured in
// hundreds of commits.
//
// # WHY IT LIVES IN scripts/dev
//
// Same reason vscode_lane_scope_test.go does: a guard placed in a package the
// lane it asserts on would run is a guard that goes silent exactly when the
// lane is narrowed. `scripts/dev` is reached by `go test ./...` under the `go`
// bucket, so this runs whether or not the gate lane does.
//
// It reuses that file's helpers (repoRoot, ciYAML, commandLines, coveredBy,
// runSelectorRe, hasPipe) rather than re-deriving them. That is deliberate:
// those helpers carry the lessons of an adversarial review that defeated the
// first version of that guard twice by exploiting line-scanning, and a second
// copy here would be a second chance to get them subtly wrong.
package dev

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// gateInputsStepPrefix identifies the step this guard asserts on. Matched on
// the step NAME because that is the only stable handle a YAML step has; every
// assertion about what it DOES reads its parsed `Run` and `If`.
const gateInputsStepPrefix = "go test (gate inputs)"

// gateInput is one file that some Go test reads, paired with the package that
// test lives in.
//
// Hand-written, and it has to be: "which non-Go files does a Go test read" is
// not mechanically derivable, and a guard that pretended otherwise would
// silently shrink to whatever the heuristic happened to catch. Each entry was
// verified by reading the test named in `gate`.
//
// Adding a gate that reads a new kind of file means adding a row here. That is
// the cost, and it is the point: the row is what stops the bucket being
// narrowed out from under the gate later.
var gateInputs = []struct {
	path string // repo-relative, must exist
	pkg  string // the package whose tests read it
	gate string // the test that reads it, for the failure message
}{
	{"CLAUDE.md", ".", "TestDocsDoNotTeachRetiredDeclarationKeywords"},
	{"integrations/arch.md", ".", "TestDocsDoNotTeachDeletedGoExtensionPoints"},
	{"docs/internal/design/extension-points.md", ".", "TestDocsDoNotTeachDeletedGoExtensionPoints"},
	{"docs/public/concepts/events.md", ".", "TestDocsDoNotReferencePrefixedConstructNames"},
	{"scripts/secrets/manifest.yaml", "component/genesis", "TestEmbeddedManifestInSync"},
	{"component/genesis/manifest.yaml", "component/genesis", "TestEmbeddedManifestInSync"},
	{"component/architecture/embedded/topology.model.json", "component/architecture", "TestArchitectureModelIsNotStale"},
	{"scripts/make/help.sh", "scripts/make", "TestHelpScript"},
	{"scripts/lib/capability.sh", "scripts/lib", "the capability-script contract test"},
	// The `**/*.memql` glob had NO row at all until the memql#3060 review,
	// so the guard asserted nothing in either direction for it -- it could
	// neither notice the glob missing from the bucket nor notice the step
	// lacking the package that reads it. Both of these are outside dsl/
	// (which has its own bucket) and outside the root package.
	{"examples/referencepack/dsl/concepts.memql", "examples/referencepack", "TestReferencePackLoadsAndExtends"},
	{"docs/public/operate/auth/per-row-authz-audit.md", "dsl", "TestPublicExamplesAreAnnotated"},
	// The embedded-asset inventory (memql#3165). embed_inventory_test.go pins
	// per-package `//go:embed` file COUNTS and lives in the root package, so
	// EVERY file it pins is a gate input -- and 68 of the 270 were in no
	// bucket this lane reads. That is a fail-open with DELAYED BLAME: the
	// PR's `ci-required` went green and the guard then fired on the next
	// author's push to main.
	//
	// One row per glob added to the bucket, and each names a file that ONLY
	// the new glob covers -- a row already matched by `**/*.md` or
	// `**/*.memql` would stay green with the new glob deleted and would
	// therefore assert nothing. `examples/deploypack/dsl/**` deliberately has
	// no row for that reason: all three of its embedded files are `.memql`
	// today, so the glob is future-proofing with nothing yet to pin.
	{"VERSION", ".", "TestEmbeddedFileCountsAreStable"},
	{"component/database/memory-nodes/migrations/20260324000000_initial_setup.up.sql", ".", "TestEmbeddedFileCountsAreStable"},
	{"component/identity/web/static/app.js", ".", "TestEmbeddedFileCountsAreStable"},
	{"component/mcp/icon.svg", ".", "TestEmbeddedFileCountsAreStable"},
	{"dsl/cognition/prompts/cognitionReply.tmpl", ".", "TestEmbeddedFileCountsAreStable"},
	{"examples/referencepack/dsl/namespace.pin", ".", "TestEmbeddedFileCountsAreStable"},
	{"integrations/integrations.json", ".", "TestEmbeddedFileCountsAreStable"},
}

// changesFilters returns the parsed filter block the `changes` job wires its
// outputs from.
//
// Resolved via the OUTPUT EXPRESSION rather than by taking whichever step
// carries filters, for the reason vscode_lane_scope_test.go records: with two
// paths-filter steps present, "last wins" reads the wrong block in both
// directions -- a decoy can mask a narrowed real one, and an unrelated second
// filter reddens a lane that is correct.
func changesFilters(t *testing.T, output string) map[string][]string {
	t.Helper()

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

	wiring, ok := doc.Jobs["changes"].Outputs[output]
	if !ok {
		t.Fatalf("the `changes` job declares no %q output. Without it nothing can consult the "+
			"bucket, so every non-Go gate input is back in no bucket and ci-required goes green "+
			"over an unverified diff (memql#2972)", output)
	}
	// Capture the output KEY as well as the step id, and require the key to
	// be the bucket we asked for.
	//
	// Matching only the step id was a hole big enough to restore memql#2972
	// in one line, found by the adversarial review of this PR: rewiring
	//
	//	gates: ${{ steps.filter.outputs.go }}
	//
	// left every assertion in this file GREEN -- the id still resolved to the
	// `filter` step, whose filters block still contains a `gates` key, and the
	// two `if:` assertions still found the string
	// "needs.changes.outputs.gates". Meanwhile at runtime a docs-only PR read
	// the `go` bucket, got false, and the lane skipped. The guard has to check
	// what the output is wired TO, not merely which step it came from.
	m := regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.outputs\.([A-Za-z0-9_-]+)`).FindStringSubmatch(wiring)
	if m == nil {
		t.Fatalf("cannot tell which step supplies the `changes` job's %q output (got %q); this "+
			"guard cannot pass vacuously", output, wiring)
	}
	if m[2] != output {
		t.Fatalf("the `changes` job's %q output is wired to the %q filter (%q). The name and the "+
			"bucket must match, or the job advertises one bucket and routes on another: a "+
			"docs-only PR would read the wrong filter, the lane would skip, and memql#2972 would "+
			"be back with every guard in this file still green.", output, m[2], wiring)
	}

	var filters string
	for _, s := range doc.Jobs["changes"].Steps {
		if s.Id == m[1] {
			filters = s.With.Filters
			break
		}
	}
	if filters == "" {
		t.Fatalf("the `changes` job's %q output names step %q, which declares no filters; this "+
			"guard cannot pass vacuously", output, m[1])
	}

	var parsed map[string][]string
	if err := yaml.Unmarshal([]byte(filters), &parsed); err != nil {
		t.Fatalf("parse the `changes` filters block: %v", err)
	}
	return parsed
}

// globRe translates one picomatch glob into a regexp.
//
// `**` crosses `/` and `*` does not -- the distinction the whole bucket turns
// on. `**/*.md` has to match `CLAUDE.md` at the ROOT as well as nested files,
// which is why the `**/` form is optional rather than requiring a leading
// segment; getting that wrong is how a filter reads as repo-wide while
// skipping the one file two gates name as their sharp edge.
func globRe(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); {
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i += 2
		case glob[i] == '*':
			b.WriteString("[^/]*")
			i++
		case glob[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
			i++
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// Every known gate input must be matched by the `gates` bucket, and must still
// exist. This is the assertion that fails against the pre-#2972 workflow.
func TestGatesBucketCoversEveryKnownGateInput(t *testing.T) {
	globs := changesFilters(t, "gates")["gates"]
	if len(globs) == 0 {
		t.Fatal("the `gates` bucket is empty, so every non-Go gate input is back in no bucket " +
			"and a PR touching only those files reports ci-required green having verified " +
			"nothing (memql#2972)")
	}

	res := make([]*regexp.Regexp, 0, len(globs))
	for _, g := range globs {
		res = append(res, globRe(g))
	}

	root := repoRoot(t)
	for _, in := range gateInputs {
		// A row naming a path that no longer exists asserts nothing, and would
		// keep asserting nothing forever.
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(in.path))); err != nil {
			t.Errorf("gateInputs names %q, which does not exist: %v. Retarget the row at whatever "+
				"replaced it rather than deleting it -- a deleted row is a gate input silently "+
				"leaving the bucket.", in.path, err)
			continue
		}

		matched := false
		for _, re := range res {
			if re.MatchString(in.path) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("%s is read by %s (package %s) but matches no `gates` glob %v.\n"+
				"A PR touching only that file would match no bucket at all, every lane would "+
				"skip, and ci-required would report success over a diff nothing verified -- "+
				"which is memql#2972 exactly.", in.path, in.gate, in.pkg, globs)
		}
	}
}

// The bucket is worthless if nothing consults it. Dropping
// `needs.changes.outputs.gates` from go-checks' `if:` leaves a condition that
// still reads plausibly while a docs-only PR stops selecting the job.
//
// This READS the condition without owning its shape: any `if:` that still
// consults the bucket passes, however it is otherwise rewritten.
func TestGoChecksRunsOnGateInputOnlyChanges(t *testing.T) {
	var wf ciWorkflow
	if err := yaml.Unmarshal(ciYAML(t), &wf); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	job, ok := wf.Jobs["go-checks"]
	if !ok {
		t.Fatal("no `go-checks` job in .github/workflows/ci.yml; if the lane was renamed, " +
			"retarget this guard rather than deleting it (memql#2972)")
	}
	if strings.TrimSpace(job.If) == "" {
		return // unconditional: stronger than required
	}
	if !strings.Contains(job.If, "needs.changes.outputs.gates") {
		t.Errorf("the `go-checks` job's `if:` no longer consults the `gates` bucket, so a PR "+
			"touching only gate inputs (.md, a manifest, the architecture model, a shell "+
			"script) would not select the lane -- and no other lane runs those gates. That is "+
			"memql#2972 restored end to end.\ngot if: %s", job.If)
	}
}

// gateInputsStep returns the parsed step that runs the gate packages.
func gateInputsStep(t *testing.T) (struct {
	Name            string `yaml:"name"`
	Run             string `yaml:"run"`
	If              string `yaml:"if"`
	ContinueOnError any    `yaml:"continue-on-error"`
}, bool) {
	t.Helper()
	var wf ciWorkflow
	if err := yaml.Unmarshal(ciYAML(t), &wf); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	for _, s := range wf.Jobs["go-checks"].Steps {
		if strings.HasPrefix(s.Name, gateInputsStepPrefix) {
			return s, true
		}
	}
	var zero struct {
		Name            string `yaml:"name"`
		Run             string `yaml:"run"`
		If              string `yaml:"if"`
		ContinueOnError any    `yaml:"continue-on-error"`
	}
	return zero, false
}

// Selecting the job is only half of it: the step has to run the packages the
// gates actually live in. A package list narrowed to, say, `./` alone leaves
// the manifest-sync and architecture-model gates unrun on the very PRs that
// change their inputs, with every other assertion here green.
func TestGateInputsStepCoversEveryGatePackage(t *testing.T) {
	step, ok := gateInputsStep(t)
	if !ok {
		t.Fatalf("no %q step in the go-checks job. Selecting the job on the `gates` bucket "+
			"without a step that runs the gate packages verifies nothing (memql#2972)",
			gateInputsStepPrefix)
	}

	var patterns []string
	for _, line := range commandLines(step.Run) {
		if !strings.Contains(line, "go test") {
			continue
		}
		cleaned := strings.ReplaceAll(strings.ReplaceAll(line, `"`, " "), `'`, " ")
		for _, m := range pkgPatternRe.FindAllStringSubmatch(cleaned, -1) {
			patterns = append(patterns, m[2])
		}
	}
	if len(patterns) == 0 {
		t.Fatal("the gate-inputs step names no package pattern; a bare `go test` tests only the " +
			"current directory (memql#2972)")
	}

	root := repoRoot(t)
	for _, in := range gateInputs {
		// The package has to exist, or "covered" is a claim about nothing.
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(in.pkg))); err != nil || !fi.IsDir() {
			t.Errorf("gateInputs row for %s names package %q, which is not a directory", in.path, in.pkg)
			continue
		}
		// coveredBy compares repo-RELATIVE directories, where the root is the
		// empty string: it strips a `./` prefix from the pattern, so the root
		// package pattern `./` normalizes to "". The gateInputs table spells
		// the same package `.` because that is how a Go import path reads.
		// Normalize here rather than in coveredBy -- that helper is shared with
		// vscode_lane_scope_test.go, and "." is not a directory it ever sees.
		dir := in.pkg
		if dir == "." {
			dir = ""
		}
		covered := false
		for _, pat := range patterns {
			if coveredBy(dir, pat) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s is in the `gates` bucket, but its gate %s lives in package %s, which "+
				"the gate-inputs step's patterns %v do not cover.\nThe bucket would select the "+
				"lane and the lane would not run the test -- a gate firing over nothing, which "+
				"is the same defect class one level in (memql#2972).",
				in.path, in.gate, in.pkg, patterns)
		}
	}
}

// A `-run` or `-skip` selector narrows scope WITHIN the packages, invisibly to
// the coverage assertion above. This repo has been bitten by exactly that:
// `make arch-model-check` ran `-run` against a test that did not exist and
// reported success having run nothing (#2923, #3003) -- the same audit sweep
// that filed this issue.
//
// GOFLAGS is checked too: `GOFLAGS=-run=TestNothing go test ./...` applies the
// selector without the flag ever appearing as an argument.
func TestGateInputsStepSelectsNoSubsetOfTests(t *testing.T) {
	step, ok := gateInputsStep(t)
	if !ok {
		t.Skip("no gate-inputs step; TestGateInputsStepCoversEveryGatePackage reports that")
	}
	for _, line := range commandLines(step.Run) {
		if !strings.Contains(line, "go test") && !strings.Contains(line, "GOFLAGS") {
			continue
		}
		if runSelectorRe.MatchString(line) {
			t.Errorf("the gate-inputs step must not narrow which tests run (-run/-skip, "+
				"including via GOFLAGS): it is invisible to the package-coverage check, and a "+
				"pattern matching nothing exits 0 reporting success (#2923).\ngot: %s", line)
		}
	}
}

// A gate whose failure is swallowed is not a gate.
//
// continue-on-error is read from the PARSED step, never from the command text
// -- it is a step key and can never appear in a command, and an adversarial
// review of the sibling guard defeated exactly that mistake by setting it as a
// sibling key while the test stayed green.
func TestGateInputsStepDoesNotSuppressFailure(t *testing.T) {
	step, ok := gateInputsStep(t)
	if !ok {
		t.Skip("no gate-inputs step; TestGateInputsStepCoversEveryGatePackage reports that")
	}
	if step.ContinueOnError != nil && step.ContinueOnError != false {
		t.Errorf("the gate-inputs step sets continue-on-error=%v; its failure would not fail "+
			"the lane (memql#2972)", step.ContinueOnError)
	}

	pipefail := false
	for _, line := range commandLines(step.Run) {
		if strings.Contains(line, "pipefail") {
			pipefail = true
			break
		}
	}
	for _, line := range commandLines(step.Run) {
		for _, esc := range []string{"|| true", "|| :", "|| exit 0"} {
			if strings.Contains(line, esc) {
				t.Errorf("the gate-inputs step must not suppress its failure exit (%q).\ngot: %s", esc, line)
			}
		}
		// A pipeline reports the LAST command's status, and GitHub runs
		// `bash -e` without `pipefail`, so `go test ... | tee out.txt` exits
		// with tee's status and a failing gate reports green.
		if strings.Contains(line, "go test") && !pipefail && hasPipe(line) {
			t.Errorf("the gate-inputs step's `go test` is piped without `pipefail`; the step "+
				"would exit with the LAST command's status, so a failing gate reports green "+
				"(memql#2972).\ngot: %s", line)
		}
	}
}

// The step's own `if:` must still consult RUN_GATES.
//
// It is deliberately conditional -- it skips when the full suite is already
// running, since `go test ./...` covers every gate package -- so this cannot
// demand an unconditional step the way the vscode guard does. What it can
// demand is that the condition still reads the variable the bucket feeds:
// an `if:` rewritten to something that never evaluates true leaves every
// assertion above green while the step runs on nothing.
func TestGateInputsStepStillConsultsItsBucket(t *testing.T) {
	step, ok := gateInputsStep(t)
	if !ok {
		t.Skip("no gate-inputs step; TestGateInputsStepCoversEveryGatePackage reports that")
	}
	if !strings.Contains(step.If, "RUN_GATES") {
		t.Errorf("the gate-inputs step's `if:` no longer consults RUN_GATES, so it no longer "+
			"tracks the `gates` bucket and would run either always or never (memql#2972)."+
			"\ngot if: %s", step.If)
	}

	var wf struct {
		Jobs map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ciYAML(t), &wf); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	runGates, ok := wf.Jobs["go-checks"].Env["RUN_GATES"]
	if !ok {
		t.Fatal("the go-checks job declares no RUN_GATES env, so the gate-inputs step's `if:` " +
			"reads an empty string and the step never runs (memql#2972)")
	}
	if !strings.Contains(runGates, "needs.changes.outputs.gates") {
		t.Errorf("RUN_GATES no longer derives from the `gates` bucket, so the step it guards is "+
			"disconnected from the paths it exists to cover.\ngot: %s", runGates)
	}
}
