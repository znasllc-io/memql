// Static guard: the lane that objects to an unrouted path must not itself be
// routed (znasllc-io/memql#3451).
//
// # The self-concealing failure
//
// `TestEveryTrackedFileReachesAConsumedBucket` (scripts/ci) is the guard that
// notices a tracked file matching no consumed `ci.yml` filter bucket. Until
// this lane existed it ran only inside `go-checks` / `go-tests`, both of which
// are selected by `needs.changes.outputs.*` -- so it was gated on the very
// routing it exists to check.
//
// The consequence is exact, and it is what memql#3451 was filed about. A PR
// whose whole diff is an unrouted file sets EVERY bucket false. Every lane's
// `if:` evaluates false, every lane skips, `ci-required` reports success over
// a diff nothing verified -- and the one test that would have objected is in
// a skipped lane. The orphan lands, and the guard fires later on an unrelated
// stranger's PR, whose author has every reason to believe the breakage is
// theirs. That is precisely the population the guard exists to catch: a file
// outside every bucket switches off its own alarm.
//
// # Why an unconditional lane and not a catch-all bucket
//
// The obvious alternative is a `**` bucket that schedules the guard. This
// repository already rejects that shape, in its own words:
// `TestNoBucketMatchesEveryTrackedFile` (scripts/ci/bucket_semantics_test.go)
// fails any bucket true for every tracked file, on the grounds that "a bucket
// that genuinely must always fire should be an unconditional lane, not a
// filter". So the remedy is the lane.
//
// It is affordable because it is small: the two guard packages run in ~3.5s
// and need no database, no npm and no CGO. The lane pays a checkout and a Go
// toolchain, not a build of the module.
//
// # WHY IT LIVES IN scripts/dev
//
// Same reason gate_inputs_lane_scope_test.go and vscode_lane_scope_test.go do:
// a guard placed only in the package the lane runs would go silent exactly
// when the lane is narrowed. This file is reached by `go test ./...` under the
// `go` bucket as well as by the lane it describes.
package dev

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pathRoutingLaneJob is the job key this guard asserts on.
const pathRoutingLaneJob = "path-routing"

// pathRoutingGuardPackages are the packages the lane must run.
//
// `scripts/ci` hosts the coverage guard itself; `scripts/dev` hosts this file
// and the ci-required needs-completeness guard. Both reason about the routing
// configuration rather than about any product code, so both belong in the lane
// that cannot be switched off by the configuration they read.
var pathRoutingGuardPackages = []string{"scripts/ci", "scripts/dev"}

// coverageGuardTest is the test whose scheduling this lane exists to
// guarantee. Named so that moving it out of scripts/ci fails here instead of
// silently leaving the lane pointed at a package that no longer objects to
// anything.
const coverageGuardTest = "TestEveryTrackedFileReachesAConsumedBucket"

// pathRoutingLane returns the parsed job, failing loudly when it is absent.
func pathRoutingLane(t *testing.T) struct {
	If              string            `yaml:"if"`
	Needs           any               `yaml:"needs"`
	ContinueOnError any               `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
	Steps           []struct {
		Name            string            `yaml:"name"`
		Run             string            `yaml:"run"`
		If              string            `yaml:"if"`
		Env             map[string]string `yaml:"env"`
		ContinueOnError any               `yaml:"continue-on-error"`
	} `yaml:"steps"`
} {
	t.Helper()
	doc := parseCIJobs(t)
	job, ok := doc.Jobs[pathRoutingLaneJob]
	if !ok {
		t.Fatalf("no %q job in .github/workflows/ci.yml.\n"+
			"That lane is the only scheduling of %s that a PR cannot switch off: a diff "+
			"consisting entirely of unrouted files sets every bucket false, so every "+
			"bucket-gated lane skips and the guard never runs on the PR that introduced "+
			"the orphan (memql#3451). If the lane was renamed, retarget this guard rather "+
			"than deleting it.", pathRoutingLaneJob, coverageGuardTest)
	}
	return job
}

// The lane must be unconditional. A `needs.changes.outputs.*` gate on THIS
// lane is the defect itself: the file that matches no bucket is exactly the
// file that would switch the gate off.
func TestPathRoutingLaneIsNotGatedOnTheRoutingItGuards(t *testing.T) {
	job := pathRoutingLane(t)

	if cond := strings.TrimSpace(job.If); cond != "" {
		t.Errorf("the %q job declares `if: %s`. This lane must carry NO condition at all: "+
			"any gate derived from the `changes` job is switched off by precisely the diff "+
			"this lane exists to object to -- an orphan matches no bucket, so every bucket "+
			"is false, so the guard does not run and the orphan merges green "+
			"(memql#3451).", pathRoutingLaneJob, cond)
	}

	// `needs:` is the same hole wearing a different key. A job that needs
	// `changes` is SKIPPED when `changes` fails or is cancelled, and
	// `ci-required` counts a skipped lane as a pass -- so the guard would go
	// quiet in the runs most likely to need it.
	switch n := job.Needs.(type) {
	case nil:
	case string:
		t.Errorf("the %q job declares `needs: %s`. It must depend on no other job: a "+
			"dependent lane is SKIPPED when its dependency fails or is cancelled, and "+
			"ci-required treats skipped as a pass (memql#3451).", pathRoutingLaneJob, n)
	case []any:
		if len(n) > 0 {
			t.Errorf("the %q job declares `needs: %v`. It must depend on no other job: a "+
				"dependent lane is SKIPPED when its dependency fails or is cancelled, and "+
				"ci-required treats skipped as a pass (memql#3451).", pathRoutingLaneJob, n)
		}
	}
}

// pathRoutingGoTestSteps returns the lane's steps whose `run:` invokes
// `go test`, read from the parsed command lines so a mention in a step `name:`
// or a comment cannot satisfy anything here.
func pathRoutingGoTestSteps(t *testing.T) []struct {
	Name            string            `yaml:"name"`
	Run             string            `yaml:"run"`
	If              string            `yaml:"if"`
	Env             map[string]string `yaml:"env"`
	ContinueOnError any               `yaml:"continue-on-error"`
} {
	t.Helper()
	var out []struct {
		Name            string            `yaml:"name"`
		Run             string            `yaml:"run"`
		If              string            `yaml:"if"`
		Env             map[string]string `yaml:"env"`
		ContinueOnError any               `yaml:"continue-on-error"`
	}
	for _, s := range pathRoutingLane(t).Steps {
		for _, line := range commandLines(s.Run) {
			if strings.Contains(line, "go test") {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// The lane must actually run the guard packages, must not narrow which tests
// run inside them, and must not swallow the failure.
func TestPathRoutingLaneRunsTheRoutingGuards(t *testing.T) {
	steps := pathRoutingGoTestSteps(t)
	if len(steps) == 0 {
		t.Fatalf("the %q job runs no `go test` at all, so it reports green having verified "+
			"nothing (memql#3451)", pathRoutingLaneJob)
	}

	for _, pkg := range pathRoutingGuardPackages {
		covered := false
		for _, s := range steps {
			for _, line := range commandLines(s.Run) {
				if !strings.Contains(line, "go test") {
					continue
				}
				for _, pat := range pkgPatternRe.FindAllString(line, -1) {
					if coveredBy(pkg, strings.TrimSpace(pat)) {
						covered = true
					}
				}
			}
		}
		if !covered {
			t.Errorf("the %q job's `go test` does not cover %s. That package hosts the "+
				"routing guards, and this lane is their only unconditional scheduling "+
				"(memql#3451).", pathRoutingLaneJob, pkg)
		}
	}

	for _, s := range steps {
		if strings.TrimSpace(s.If) != "" {
			t.Errorf("the %q job's `go test` step is gated on `if: %s`. A step condition "+
				"reports green having run nothing, which is the same hole one level down "+
				"(memql#3451).\nstep: %s", pathRoutingLaneJob, s.If, s.Name)
		}
		if s.ContinueOnError != nil && s.ContinueOnError != false {
			t.Errorf("the %q job's `go test` step sets continue-on-error=%v; its failure "+
				"would not fail the lane.\nstep: %s", pathRoutingLaneJob, s.ContinueOnError, s.Name)
		}
		pipefail := false
		for _, line := range commandLines(s.Run) {
			if strings.Contains(line, "pipefail") {
				pipefail = true
				break
			}
		}
		for _, line := range commandLines(s.Run) {
			if runSelectorRe.MatchString(line) {
				t.Errorf("the %q job's `go test` narrows which tests run (-run/-skip). A "+
					"selector matching nothing exits 0 having tested nothing (#2923).\ngot: %s",
					pathRoutingLaneJob, line)
			}
			for _, esc := range []string{"|| true", "|| :", "|| exit 0"} {
				if strings.Contains(line, esc) {
					t.Errorf("the %q job's `go test` suppresses its failure exit (%q).\ngot: %s",
						pathRoutingLaneJob, esc, line)
				}
			}
			if strings.Contains(line, "go test") && !pipefail && hasPipe(line) {
				t.Errorf("the %q job's `go test` is piped without `pipefail`, so the step "+
					"exits with the LAST command's status and a failing guard reports "+
					"green.\ngot: %s", pathRoutingLaneJob, line)
			}
		}
	}
}

// The lane is worth nothing if the guard has moved out from under it. Derived
// from the source rather than assumed, so relocating the coverage guard fails
// here instead of leaving the lane running a package that objects to nothing.
func TestCoverageGuardStillLivesInAPackageTheLaneRuns(t *testing.T) {
	root := repoRoot(t)

	found := ""
	for _, pkg := range pathRoutingGuardPackages {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(pkg)))
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pkg), e.Name()))
			if err != nil {
				t.Fatalf("read %s/%s: %v", pkg, e.Name(), err)
			}
			if strings.Contains(string(raw), "func "+coverageGuardTest+"(") {
				found = pkg + "/" + e.Name()
			}
		}
	}
	if found == "" {
		t.Errorf("no file in %v declares func %s. The lane runs those packages precisely "+
			"to schedule that guard unconditionally; if the guard moved, move the lane's "+
			"package list with it rather than leaving a lane that verifies nothing "+
			"(memql#3451).", pathRoutingGuardPackages, coverageGuardTest)
	}
}
