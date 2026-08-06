// Static guard: the db-gated package set is defined once, in effect
// (znasllc-io/memql#3160).
//
// # Why the set appears in two files at all
//
// `go-checks` subtracts the db-gated trees; `db-tests` runs exactly those
// trees. Ideally both would read one file. They cannot, and the reason is
// worth recording so nobody "fixes" it:
//
// scripts/cidb/dbgate_test.go is an 1100-line gate that derives the db-tests
// lane's package selector by parsing literal `./`-prefixed arguments out of
// that step's `run:` block. `source` and `.` are both in its
// shellControlKeywords, so making db-tests source a shared script either
// empties the derived package list ("parsed no `go test ./...` package
// arguments") or marks the run block non-plain -- disarming the gate that
// exists because these very suites once rotted unnoticed (memql#2342).
//
// Trading a live regression gate for tidiness is a bad trade. So db-tests
// keeps its literal arguments, scripts/ci/db-gated-packages.sh holds the
// canonical set for go-checks to subtract, and THIS test makes the duplication
// unable to drift. "Defined in one place" is the property that matters; a
// single literal is only one way to get it.
//
// # What breaks if this test is deleted
//
// Silently, and in the worse direction: a tree added to db-tests but not to
// the script keeps running in BOTH lanes (the duplication this task removed
// comes back), and a tree removed from db-tests but left in the script stops
// running in EITHER lane -- untested code, green gate.
package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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

// dbgatedTreesFromScript reads the canonical set out of the shell array in
// db-gated-packages.sh.
//
// Parsed rather than executed: running the script would need a shell and a
// working `go list`, and this assertion is about the literal set, not about
// the complement it computes.
func dbgatedTreesFromScript(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(dbgatedRepoRoot(t), "scripts", "ci", "db-gated-packages.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(raw)
	start := strings.Index(body, "readonly DB_GATED_TREES=(")
	if start < 0 {
		t.Fatalf("%s: no `readonly DB_GATED_TREES=(` array found -- this guard "+
			"cannot verify what it was written to verify, so it fails closed", path)
	}
	rest := body[start:]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("%s: unterminated DB_GATED_TREES array", path)
	}
	var trees []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(rest[:end], -1) {
		trees = append(trees, m[1])
	}
	if len(trees) == 0 {
		t.Fatalf("%s: DB_GATED_TREES is empty", path)
	}
	sort.Strings(trees)
	return trees
}

// dbgatedTreesFromCI reads the trees the db-tests lane actually passes to
// `go test`, from ci.yml.
func dbgatedTreesFromCI(t *testing.T) []string {
	t.Helper()
	path := filepath.Join(dbgatedRepoRoot(t), ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf struct {
		Jobs map[string]struct {
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

	seen := map[string]bool{}
	var trees []string
	for _, s := range job.Steps {
		if !strings.Contains(s.Run, "go test") {
			continue
		}
		for _, f := range strings.Fields(s.Run) {
			// `./component/memql/...` -> `component/memql`
			if !strings.HasPrefix(f, "./") {
				continue
			}
			tree := strings.TrimSuffix(strings.TrimSuffix(f, "..."), "/")
			tree = strings.TrimPrefix(tree, "./")
			if tree == "" || seen[tree] {
				continue
			}
			seen[tree] = true
			trees = append(trees, tree)
		}
	}
	if len(trees) == 0 {
		t.Fatal("ci.yml db-tests lane names no `./`-prefixed package arguments; either " +
			"the lane stopped running packages or this guard's parsing broke -- both are failures")
	}
	sort.Strings(trees)
	return trees
}

// TestDBGatedTreesMatchTheDBTestsLane is the anti-drift guard.
func TestDBGatedTreesMatchTheDBTestsLane(t *testing.T) {
	script := dbgatedTreesFromScript(t)
	ci := dbgatedTreesFromCI(t)

	if strings.Join(script, ",") == strings.Join(ci, ",") {
		return
	}
	t.Errorf("the db-gated tree set has drifted.\n"+
		"  scripts/ci/db-gated-packages.sh: %v\n"+
		"  ci.yml db-tests lane:            %v\n\n"+
		"go-checks subtracts the script's set and db-tests runs its own, so a mismatch "+
		"means either a tree runs in BOTH lanes (the duplication memql#3160 removed) or "+
		"in NEITHER (untested code behind a green gate). Make them identical.",
		script, ci)
}
