package cidb

// Read the same manifest, source graph, policy and compiler that production
// uses. A database package must survive selection, sharding AND the real
// command builder; checking a retired Actions matrix cannot establish that.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

func nativeSpec(t *testing.T) *pipelines.Spec {
	t.Helper()
	manifest, err := packages.ReadManifest(os.DirFS(repoRoot(t)))
	if err != nil {
		t.Fatal(err)
	}
	if refusal := pipelines.Validate(manifest.Pipeline); refusal != nil {
		t.Fatal(refusal)
	}
	return manifest.Pipeline
}

type databaseStep struct {
	stage string
	step  pipelines.StepSpec
}

func nativeDatabaseStep(t *testing.T, spec *pipelines.Spec) databaseStep {
	t.Helper()
	for _, stage := range spec.Stages {
		for _, step := range stage.Steps {
			if step.Name != dbTestsJob {
				continue
			}
			if len(stage.On) > 0 && !(slices.Contains(stage.On, "pull_request") && slices.Contains(stage.On, "merge_group")) {
				t.Fatalf("database coverage does not gate both PRs and merge candidates: %v", stage.On)
			}
			if step.Packages != pipelines.PackagesAffected || step.Only != pipelines.OnlyDBGated || step.Shards < 1 || step.When != nil || !slices.Contains(step.Services, "postgres") {
				t.Fatalf("database step lost its selected partition or database: %+v", step)
			}
			return databaseStep{stage.Name, step}
		}
	}
	t.Fatal("native pipeline has no database step")
	return databaseStep{}
}

func nativeGraph(t *testing.T) *pipelines.Graph {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{}
	for _, name := range strings.Split(string(raw), "\x00") {
		if filepath.Base(name) != "go.mod" && !strings.HasSuffix(name, ".go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		tree[name] = &fstest.MapFile{Data: data}
	}
	graph, err := pipelines.ScanGoTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Packages()) < 100 || len(graph.Incomplete()) != 0 {
		t.Fatalf("incomplete repository graph: %v", graph.Incomplete())
	}
	return graph
}

func fullNativePlan(t *testing.T, spec *pipelines.Spec, graph *pipelines.Graph) pipelines.Plan {
	t.Helper()
	in := pipelines.CompileInput{
		Mode: pipelines.ModeFull, Event: pipelines.EventMergeGroup, Compute: pipelines.ComputeClusterAndFleet,
		Selector:       pipelines.GraphSelector(graph, pipelines.Selection{Full: true}),
		StageSelection: &pipelines.StageSelection{}, BucketSelection: &pipelines.BucketSelection{},
		PackagePolicies: map[string]pipelines.PackagePolicy{},
	}
	for bucket := range spec.Select.Buckets {
		in.BucketSelection.Included = append(in.BucketSelection.Included, bucket)
	}
	for _, stage := range spec.Stages {
		on := stage.On
		if on == nil {
			on = []string{}
		}
		value, err := workflowhost.Run(context.Background(), "pipelineStageIncluded", map[string]any{
			"event": "merge_group", "mode": "full", "on": on, "runSchedule": "", "stageSchedule": stage.Schedule,
		}, workflowhost.Options{})
		included, ok := value.(bool)
		if err != nil || !ok {
			t.Fatalf("stage policy: %v %v", value, err)
		}
		if included {
			in.StageSelection.Included = append(in.StageSelection.Included, stage.Name)
		}
		for _, step := range stage.Steps {
			if step.Packages == "" {
				continue
			}
			value, err := workflowhost.Run(context.Background(), "pipelinePackageStepSelection", map[string]any{
				"mode": "full", "packages": step.Packages, "selectionFull": true, "only": step.Only,
			}, workflowhost.Options{})
			policy, ok := value.(map[string]any)
			if err != nil || !ok {
				t.Fatalf("package policy: %v %v", value, err)
			}
			coverage, coverageOK := policy["coverage"].(string)
			filter, filterOK := policy["filter"].(string)
			if !coverageOK || !filterOK {
				t.Fatal("malformed package policy")
			}
			in.PackagePolicies[pipelines.StepKey(stage.Name, step.Name)] = pipelines.PackagePolicy{Coverage: coverage, Filter: filter}
		}
	}
	plan, refusal := pipelines.Compile(spec, in)
	if refusal != nil {
		t.Fatal(refusal)
	}
	return plan
}

func TestNativeSelectionUsesTheCanonicalDatabaseTrees(t *testing.T) {
	spec := nativeSpec(t)
	nativeDatabaseStep(t, spec)
	var patterns []string
	for _, tree := range spec.Select.DBGated {
		patterns = append(patterns, "./"+tree+"/...")
	}
	canonical := canonicalPatterns(t, repoRoot(t))
	slices.Sort(patterns)
	slices.Sort(canonical)
	if !slices.Equal(patterns, canonical) {
		t.Fatalf("native DB trees %v differ from canonical %v", patterns, canonical)
	}
}

func TestPlannerPlacesEveryProvisionedPackage(t *testing.T) {
	spec, graph := nativeSpec(t), nativeGraph(t)
	declared := nativeDatabaseStep(t, spec)
	plan := fullNativePlan(t, spec, graph)
	placed := map[string]int{}
	var shards [][]string
	selector := pipelines.GraphSelector(graph, pipelines.Selection{Full: true})
	for _, step := range plan.Steps() {
		if step.Stage != declared.stage || !strings.HasPrefix(step.Key, declared.stage+"."+dbTestsJob+"#") && step.Key != declared.stage+"."+dbTestsJob {
			continue
		}
		if step.Skip != nil || len(step.Packages) == 0 || step.Services["postgres"].Image == "" {
			t.Fatalf("empty or unprovisioned DB shard: %+v", step)
		}
		shards = append(shards, step.Packages)
		for _, pkg := range step.Packages {
			placed[selector.DirOf(pkg)]++
		}
	}
	if len(shards) == 0 {
		t.Fatal("compiler produced no database shards")
	}
	for dir, n := range placed {
		if n != 1 {
			t.Errorf("%s executes %d times", dir, n)
		}
	}
	root := repoRoot(t)
	for _, dir := range provisionedPkgs(t, root) {
		if placed[dir] != 1 {
			t.Errorf("%s has an EnsureSchema TestMain but reaches %d database shards", dir, placed[dir])
		}
	}
	for _, dbt := range scanDBGatedTests(t, root) {
		if placed[dbt.dir] != 1 {
			t.Errorf("%s (%s) has DB tests but reaches %d database shards", dbt.dir, dbt.name, placed[dbt.dir])
			break
		}
	}

	// Ask the real native budget runner to construct every shard's commands.
	// Assert package preservation after the final partition, not just before it.
	for _, shard := range shards {
		input, _ := json.Marshal(shard)
		cmd := exec.Command("python3", "-c", `import importlib.util,json,sys,yaml
from pathlib import Path
p=Path('scripts/ci/pipeline-db-tests.py')
s=importlib.util.spec_from_file_location('runner',p)
m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
b=yaml.safe_load(m.BUDGETS_PATH.read_text())
print(json.dumps([command for _,command in m.commands(json.load(sys.stdin),m.read_classes(b),m.read_parallelism(b))]))`)
		cmd.Dir = root
		cmd.Stdin = strings.NewReader(string(input))
		output, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		var commands [][]string
		if err := json.Unmarshal(output, &commands); err != nil {
			t.Fatal(err)
		}
		var actual []string
		for _, command := range commands {
			if len(command) < 7 || !slices.Equal(command[:4], []string{"go", "test", "-count=1", "-p=1"}) || !strings.HasPrefix(command[4], "-parallel=") || !strings.HasPrefix(command[5], "-timeout=") {
				t.Fatalf("native runner stopped running uncached bounded tests: %v", command)
			}
			actual = append(actual, command[6:]...)
		}
		slices.Sort(actual)
		expected := slices.Clone(shard)
		slices.Sort(expected)
		if !slices.Equal(actual, expected) {
			t.Fatalf("native commands changed the compiler's packages: got %v want %v", actual, expected)
		}
	}
}
