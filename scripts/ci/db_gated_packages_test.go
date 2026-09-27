// Static guard: the db-gated package set is defined once (znasllc-io/memql#3160,
// memql#5485).
//
// # One copy, and how it used to be two
//
// `go-tests` runs the complement of the db-gated trees; `db-tests` runs exactly
// those trees. Until memql#5485 the set lived in two files -- the DB_GATED_TREES
// array in scripts/ci/db-gated-packages.sh, and literal `./<tree>/...`
// arguments on db-tests' `go test` lines -- because scripts/cidb derived the
// lane's selector from those literals and refused a step that sourced a
// script. This test held the two copies equal.
//
// The planner removed the second copy. db-tests is now a matrix of shards over
// `needs.plan.outputs.db_matrix`, and scripts/ci/affected builds that matrix by
// RUNNING db-gated-packages.sh: the array is the only list. scripts/cidb reads
// the same script for its coverage assertions, and runs the real planner to
// prove every provisioned package lands in a shard.
//
// # What this guards now
//
// The second copy coming back. A literal package argument on a db-tests
// `go test` line would run that tree once PER SHARD beside the planner's
// placement of it -- the duplication memql#3160 removed, multiplied -- while
// looking like an ordinary edit. scripts/cidb refuses the same shape; this is
// the check a reader of the set's own file finds first.
package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func dbgatedRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// plannedPackages matches the planner's package argument, spaces optional.
var plannedPackages = regexp.MustCompile(`\$\{\{\s*matrix\.packages\s*\}\}`)

// TestDBTestsLaneTakesItsSetFromThePlanner is the single-source guard.
func TestDBTestsLaneTakesItsSetFromThePlanner(t *testing.T) {
	path := filepath.Join(dbgatedRepoRoot(t), ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix yaml.Node `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	job, ok := wf.Jobs["db-tests"]
	if !ok {
		t.Fatal("ci.yml no longer defines a `db-tests` job; if the lane was renamed, " +
			"retarget this guard rather than deleting it")
	}
	if got := strings.Join(strings.Fields(job.Strategy.Matrix.Value), ""); got != "${{fromJSON(needs.plan.outputs.db_matrix)}}" {
		t.Errorf("db-tests' strategy.matrix is %q, not the planner's db_matrix: the lane's packages "+
			"would come from somewhere other than db-gated-packages.sh", job.Strategy.Matrix.Value)
	}

	planned := 0
	for _, s := range job.Steps {
		for _, line := range strings.Split(s.Run, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "go test") {
				continue
			}
			if plannedPackages.MatchString(line) {
				planned++
			}
			for _, f := range strings.Fields(line) {
				if strings.HasPrefix(f, "./") {
					t.Errorf("db-tests names %q literally (%q). The set is DB_GATED_TREES in "+
						"scripts/ci/db-gated-packages.sh, placed into shards by the planner; a literal "+
						"here is a second copy that runs once per shard (memql#3160, memql#5485).", f, line)
				}
			}
		}
	}
	if planned == 0 {
		t.Error("no db-tests `go test` line takes ${{matrix.packages}}: the lane runs nothing the " +
			"planner placed, or this guard's parsing broke -- both are failures")
	}
}
