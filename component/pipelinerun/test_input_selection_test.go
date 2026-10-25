package pipelinerun

import (
	"context"
	"os"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

func TestDeclaredInputsUsePinnedDSLAndActualRepositoryLanes(t *testing.T) {
	ctx := context.Background()
	tree := os.DirFS("../..")
	manifest, err := packages.ReadManifest(tree)
	if err != nil {
		t.Fatal(err)
	}
	spec := manifest.Pipeline
	if spec == nil || spec.Select == nil {
		t.Fatal("actual repository has no pipeline selection")
	}
	// Qualify the exact deferred declaration over the real manifest without
	// putting an unknown key into a PR that the installed older engine must
	// parse. The opt-in follows deployment of this native capability.
	fixture, err := packages.ReadManifest(fstest.MapFS{
		packages.ManifestName: {Data: mustReadDeclaredInputFixture(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Pipeline == nil || fixture.Pipeline.Select == nil || len(fixture.Pipeline.Select.TestInputs) != 5 {
		t.Fatal("the explicit five-input opt-in fixture changed")
	}
	if len(spec.Select.TestInputs) > 0 && !reflect.DeepEqual(spec.Select.TestInputs, fixture.Pipeline.Select.TestInputs) {
		t.Fatal("source declarations differ from the reviewed opt-in")
	}
	selectionSpec := *spec.Select
	selectionSpec.TestInputs = slices.Clone(fixture.Pipeline.Select.TestInputs)
	spec.Select = &selectionSpec
	if !reflect.DeepEqual(spec.Select.Buckets["gates"], []string{"**"}) {
		t.Fatal("all-input contract gates changed")
	}
	graph, err := pipelines.ScanGoTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if paths := graph.Incomplete(); len(paths) != 0 {
		t.Fatalf("actual repository graph incomplete: %v", paths)
	}
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	stages, err := dr.selectStages(ctx, spec, pipelines.EventPullRequest, pipelines.ModeAffected, "")
	if err != nil {
		t.Fatal(err)
	}
	compile := func(t *testing.T, changed []string, declaration bool) pipelines.Plan {
		t.Helper()
		var declarations []string
		if declaration {
			declarations = spec.Select.TestInputs
		}
		facts, err := pipelines.AnalyzeSelectionWithTestInputs(graph, changed, true, spec.Select.Full, declarations)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := dr.selectPackageCoverage(ctx, facts, pipelines.ModeAffected)
		if err != nil {
			t.Fatal(err)
		}
		if decision.Full {
			t.Fatalf("known inputs unexpectedly full: %+v facts=%+v", decision, facts.Paths)
		}
		selection, err := pipelines.ResolveSelection(graph, facts, decision)
		if err != nil {
			t.Fatal(err)
		}
		buckets, err := dr.selectBuckets(ctx, spec, pipelines.ModeAffected, changed, true, selection.Full)
		if err != nil {
			t.Fatal(err)
		}
		policies, err := dr.selectPackagePolicies(ctx, spec, pipelines.ModeAffected, selection)
		if err != nil {
			t.Fatal(err)
		}
		plan, refusal := pipelines.Compile(spec, pipelines.CompileInput{
			Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, Compute: pipelines.ComputeClusterAndFleet,
			Selector: pipelines.GraphSelector(graph, selection), PackagePolicies: policies, BucketSelection: &buckets, StageSelection: &stages,
		})
		if refusal != nil {
			t.Fatal(refusal)
		}
		return plan
	}
	for _, tc := range []struct {
		name  string
		paths []string
	}{
		{"actual PR6291", []string{"docs/public/operate/pipelines.md", "docs/public/operate/packages.md", "docs/internal/ops/harness-decisions.md"}},
		{"separate Ask qualification", []string{"clients/os/README.md", "docs/public/operate/pipelines-substrate.md"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := compile(t, tc.paths, false)
			narrow := compile(t, tc.paths, true)
			for _, key := range []string{"cockpit.docker-integration", "cockpit.computeruse", "delivery-artifacts.provider-qualification"} {
				before, after := findPlanStep(t, old, key), findPlanStep(t, narrow, key)
				if before.Skip != nil || len(before.Packages) == 0 {
					t.Fatalf("baseline does not reproduce %s fanout: %+v", key, before)
				}
				if after.Skip == nil || len(after.Packages) != 0 {
					t.Fatalf("declared prose still selects %s: %+v", key, after)
				}
			}
			for _, key := range []string{"checks.go-checks", "checks.path-routing", "gates.gate-inputs"} {
				before, after := findPlanStep(t, old, key), findPlanStep(t, narrow, key)
				if key == "gates.gate-inputs" {
					// Affected coverage changes shard counts, so the concrete
					// dependency keys differ. Both gates must still wait for
					// every compiled test, including every shard and skip.
					assertGateWaitsForAllTests(t, old, spec)
					assertGateWaitsForAllTests(t, narrow, spec)
					before.DependsOn, after.DependsOn = nil, nil
				}
				if before.Skip != nil || after.Skip != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("required source gate %s changed: before=%+v after=%+v", key, before, after)
				}
			}
		})
	}
	for _, p := range []string{"docs/public/language/memql.md", "docs/public/operate/env-vars.md"} {
		t.Run(p, func(t *testing.T) {
			// The embedded guide and undeclared inputs keep their existing
			// runtime/importer coverage, even beside a declared prose file.
			plan := compile(t, []string{p, "docs/public/operate/pipelines.md"}, true)
			for _, key := range []string{"cockpit.computeruse", "delivery-artifacts.provider-qualification"} {
				step := findPlanStep(t, plan, key)
				if step.Skip != nil || len(step.Packages) == 0 {
					t.Fatalf("%s lost %s: %+v", p, key, step)
				}
			}
		})
	}
}

func assertGateWaitsForAllTests(t *testing.T, plan pipelines.Plan, spec *pipelines.Spec) {
	t.Helper()
	var declared []string
	for _, stage := range spec.Stages {
		if stage.Name == "tests" {
			for _, step := range stage.Steps {
				declared = append(declared, step.Name)
			}
		}
	}
	var keys, names []string
	for _, step := range plan.Steps() {
		if step.Stage == "tests" {
			keys = append(keys, step.Key)
			names = append(names, step.Name)
		}
	}
	// Shards retain their declaration's name. No declared test family may
	// disappear when its package set shrinks; an empty set stays a skip.
	if len(declared) == 0 || !slices.Equal(slices.Compact(names), declared) {
		t.Fatalf("compiled test declarations changed: got %v want %v", names, declared)
	}
	gate := findPlanStep(t, plan, "gates.gate-inputs")
	if len(keys) == 0 || !slices.Equal(gate.DependsOn, keys) {
		t.Fatalf("gate does not wait for all compiled tests: got %v want %v", gate.DependsOn, keys)
	}
}

func mustReadDeclaredInputFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/declared-test-inputs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDeclaredInputPolicyUncertaintyAndLegacyResponse(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	tree := fstest.MapFS{
		"go.mod":     {Data: []byte("module example.test/prose\n")},
		"a/a.go":     {Data: []byte("package a\n")},
		"a/guide.md": {Data: []byte("prose")},
		"b/b.go":     {Data: []byte("package b\nimport _ \"example.test/prose/a\"\n")},
	}
	g, err := pipelines.ScanGoTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/guide.md", "a/deleted.md"} {
		facts, err := pipelines.AnalyzeSelectionWithTestInputs(g, []string{p}, true, nil, []string{p})
		if err != nil {
			t.Fatal(err)
		}
		decision, err := dr.selectPackageCoverage(context.Background(), facts, pipelines.ModeAffected)
		if err != nil {
			t.Fatal(err)
		}
		if p == "a/deleted.md" {
			if !decision.Full || len(decision.TestInputs) != 0 {
				t.Fatalf("deleted input policy=%+v", decision)
			}
			continue
		}
		if decision.Full || !reflect.DeepEqual(decision.TestInputs, []string{p}) {
			t.Fatalf("eligible input policy=%+v", decision)
		}
		legacy, err := packageSelectionDecision(map[string]any{"full": false, "reason": "legacy response"})
		if err != nil {
			t.Fatal(err)
		}
		selection, err := pipelines.ResolveSelection(g, facts, legacy)
		if err != nil || !slices.Contains(selection.Packages, "example.test/prose/b") {
			t.Fatalf("legacy response narrowed: %+v %v", selection, err)
		}
	}
}

func TestDeclaredInputPolicyRejectsMalformedResponse(t *testing.T) {
	for _, raw := range []any{nil, "a/readme.md", []any{nil}, []any{1}, map[string]any{"path": "a/readme.md"}} {
		if got, err := packageSelectionDecision(map[string]any{"full": false, "testInputs": raw}); err == nil {
			t.Fatalf("accepted malformed testInputs %T: %+v", raw, got)
		}
	}
	for _, raw := range []any{[]any{}, []any{"a/readme.md"}} {
		if _, err := packageSelectionDecision(map[string]any{"full": false, "testInputs": raw}); err != nil {
			t.Fatalf("valid array: %v", err)
		}
	}
}
