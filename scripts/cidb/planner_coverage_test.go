package cidb

// planner_coverage_test.go -- the runtime half of this gate for the planned
// db-tests lane (memql#5485).
//
// The static assertions in dbgate_test.go read `run:` text. Once the lane is a
// matrix of shards chosen at run time, the text says `${{matrix.packages}}` and
// nothing more, so the question "does every provisioned package actually run"
// moves to the planner. This file asks it directly: it runs the REAL planner,
// in full mode, over the REAL tree, with the class table ci.yml itself carries,
// and checks every provisioned package lands in a db shard. A planner that
// dropped one -- a class table that cannot place it, a partition that loses it
// -- reds this test on every pull request, including the ones where the lane
// itself is skipped.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/scripts/ci/selection"
)

const modulePath = "github.com/znasllc-io/memql"

// planJob is the part of ci.yml's plan job this test reads.
type planJob struct {
	Outputs map[string]string `yaml:"outputs"`
	Steps   []struct {
		ID  string            `yaml:"id"`
		Run string            `yaml:"run"`
		Env map[string]string `yaml:"env"`
	} `yaml:"steps"`
}

func loadPlanJob(t *testing.T, root string) planJob {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", ciWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]planJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatal(err)
	}
	job, ok := wf.Jobs["plan"]
	if !ok {
		t.Fatal("ci.yml has no `plan` job, yet db-tests reads its matrix (memql#5485)")
	}
	return job
}

// TestThePlannerFeedsTheDBTestsMatrix holds the wiring between the two jobs:
// the output db-tests reads is the planner step's, and that step runs the
// planner.
func TestThePlannerFeedsTheDBTestsMatrix(t *testing.T) {
	job := loadPlanJob(t, repoRoot(t))
	if got := strings.Join(strings.Fields(job.Outputs["db_matrix"]), ""); got != "${{steps.plan.outputs.db_matrix}}" {
		t.Errorf("plan.outputs.db_matrix = %q; it must be the planner step's own output", job.Outputs["db_matrix"])
	}
	found := false
	for _, s := range job.Steps {
		if s.ID == "plan" {
			found = strings.Contains(s.Run, "go run ./scripts/ci/affected plan")
		}
	}
	if !found {
		t.Error("no step `id: plan` runs `go run ./scripts/ci/affected plan`; db-tests would read a matrix nothing computed")
	}
}

// TestPlannerPlacesEveryProvisionedPackage runs the planner in full mode --
// what every push and merge_group run gets -- and asserts every package that
// earned an EnsureSchema TestMain, and every package holding a db-gated test,
// lands in a db-tests shard.
func TestPlannerPlacesEveryProvisionedPackage(t *testing.T) {
	root := repoRoot(t)
	job := loadPlanJob(t, root)
	var classesText string
	for _, s := range job.Steps {
		if s.ID == "plan" {
			classesText = s.Env["CLASSES"]
		}
	}
	classes, err := selection.ParseClasses(classesText)
	if err != nil {
		t.Fatalf("the plan step's CLASSES table: %v", err)
	}
	tags, err := selection.NodeTags(root)
	if err != nil {
		t.Fatal(err)
	}
	g, err := selection.LoadGraph(context.Background(), root, modulePath, tags, selection.GoList(root, modulePath+"/..."))
	if err != nil {
		t.Fatalf("loading the package graph: %v", err)
	}
	trees := scriptLines(t, root, "--trees")
	complement := scriptLines(t, root, "--complement")
	f, err := os.Open(filepath.Join(root, "scripts", "ci", "shard-timings.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	timings, err := selection.ReadTimings(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := selection.MakePlan(selection.PlanInput{
		Event: "push", Graph: g, DBTrees: trees, Complement: complement,
		Classes: classes, Timings: timings, CPUs: 4,
		GatePatterns: []string{"./"}, TagTrees: []string{"app"},
	})
	if err != nil {
		t.Fatalf("the planner refused a full plan of the real tree: %v", err)
	}
	placed := map[string]bool{}
	for _, s := range plan.DBShards {
		for _, d := range s.Dirs {
			placed[d] = true
		}
	}
	for _, dir := range provisionedPkgs(t, root) {
		if !placed[dir] {
			t.Errorf("%s has an EnsureSchema TestMain but the planner put it in no db-tests shard: its DB assertions would run nowhere", dir)
		}
	}
	for _, dbt := range scanDBGatedTests(t, root) {
		if !placed[dbt.dir] {
			t.Errorf("%s (%s) holds db-gated tests but the planner put it in no db-tests shard", dbt.dir, dbt.name)
			break
		}
	}
	if len(placed) == 0 {
		t.Fatal("the planner placed no package in a db shard; every assertion above would pass vacuously")
	}
}

func scriptLines(t *testing.T, root, mode string) []string {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "ci", "db-gated-packages.sh"), mode)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("db-gated-packages.sh %s: %v", mode, err)
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
