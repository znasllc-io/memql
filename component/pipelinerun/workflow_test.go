package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/pipelines"
)

const independentWorkflow = `
use pipelines.actions.{ executePipelineStep }
@template
automation independentPipelineChecks {
  args { stages []any! }
  for stage in args.stages {
    for item in stage.steps {
      action executePipelineStep(stepKey: item.key)
    }
  }
}`

func workflowSource(t *testing.T, source string) *automations.Automation {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(source, "workflow-test.memql")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func workflowLoader(definitions ...*automations.Automation) func(string) (*automations.Automation, error) {
	return func(name string) (*automations.Automation, error) {
		for _, a := range definitions {
			if a.Name == name {
				return a, nil
			}
		}
		return loadPipelineWorkflow(name)
	}
}

func manifestWorkflow(manifest, name string) string {
	return strings.Replace(manifest, "pipeline:\n", "pipeline:\n  workflow: "+name+"\n", 1)
}

func TestASeparatelyNamedDSLWorkflowChangesFailurePolicy(t *testing.T) {
	a := workflowSource(t, independentWorkflow)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	dh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.StepKey == "checks.vet" {
			return pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: 1}, nil
		}
		return passed(req), nil
	}
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	if got := dh.exec.sentKeys(); !slices.Equal(got, []string{"checks.vet", "tests.unit", "tests.lint"}) {
		t.Fatalf("the independent DSL workflow must continue after a failure: %v", got)
	}
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure {
		t.Fatalf("continuing did not make the failed check pass: %+v", got)
	}
}

func TestAPipelineWorkflowComposesTheSealedDefault(t *testing.T) {
	a := workflowSource(t, `use pipelines.automations.{ runPipelineStages }
@template
automation wrappedPipelineChecks {
  args { stages []any! }
  automation runPipelineStages(stages: args.stages)
}`)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess || len(dh.exec.sentKeys()) != 3 {
		t.Fatalf("composed workflow: %+v; steps %v", got, dh.exec.sentKeys())
	}
}

func TestPipelineWorkflowCannotExecuteAnUncompiledStep(t *testing.T) {
	a := workflowSource(t, `use pipelines.actions.{ executePipelineStep }
@template
automation forgedPipelineStep {
  args { stages []any! }
  for stage in args.stages {
    action executePipelineStep(stepKey: "another-run.step")
  }
}`)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || len(dh.exec.sentKeys()) != 0 {
		t.Fatalf("forged step ran: %+v", got)
	}
}

func TestPipelineWorkflowCannotInvokeUnscopedEffects(t *testing.T) {
	a := workflowSource(t, `@template
automation unscopedPipelineEffect {
  args { stages []any! }
  for stage in args.stages {
    builtin pipelinesTrigger(inboundRequestId: stage.name)
  }
}`)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.RefusalCode != pipelines.CodeStageInvalid || got.WorkRunID != "" || len(dh.exec.sentKeys()) != 0 {
		t.Fatalf("unscoped workflow was not refused before work: %+v", got)
	}
}

func TestRepeatedWorkflowActionsKeepOneReceipt(t *testing.T) {
	a := workflowSource(t, strings.Replace(independentWorkflow,
		"action executePipelineStep(stepKey: item.key)",
		"parallel { branch first { action executePipelineStep(stepKey: item.key) } branch second { action executePipelineStep(stepKey: item.key) } }", 1))
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess || len(dh.exec.sentKeys()) != 3 {
		t.Fatalf("repeated action repeated an effect: %+v; %v", got, dh.exec.sentKeys())
	}
	for _, key := range dh.exec.sentKeys() {
		if n := len(dh.work.receiptsOf(key)); n != 1 {
			t.Fatalf("%s has %d receipts", key, n)
		}
	}
}

func TestPipelineWorkflowOperationsRefuseOutsideAClaimedRun(t *testing.T) {
	i := New(Deps{})
	for _, c := range i.Capabilities() {
		if !slices.Contains([]string{"workflowFacts", "skipStep", "executeStep", "reportProgress"}, c.Name) {
			continue
		}
		if _, err := c.Handler(context.Background(), map[string]any{"stepKey": "checks.vet"}, 0); err == nil || !strings.Contains(err.Error(), "active claimed run") {
			t.Fatalf("%s outside a claim: %v", c.Name, err)
		}
	}
}

func TestPipelineWorkflowIdentityIncludesTransitiveDefinitions(t *testing.T) {
	wrapper := workflowSource(t, `use pipelines.automations.{ runPipelineStages }
@template
automation wrappedPipelineChecks {
  args { stages []any! }
  automation runPipelineStages(stages: args.stages)
}`)
	one := &runDriver{d: Deps{LoadWorkflow: workflowLoader(wrapper)}}
	if err := one.prepareWorkflow(wrapper.Name); err != nil {
		t.Fatal(err)
	}
	child := workflowSource(t, strings.Replace(independentWorkflow, "independentPipelineChecks", defaultPipelineWorkflow, 1))
	two := &runDriver{d: Deps{LoadWorkflow: workflowLoader(wrapper, child)}}
	if err := two.prepareWorkflow(wrapper.Name); err != nil {
		t.Fatal(err)
	}
	if one.workflowIdentity() == two.workflowIdentity() {
		t.Fatal("changed child workflow retained the parent's recovery identity")
	}
	step := pipelines.Step{Key: "checks.vet"}
	one.d.EngineRevision = func() string { return "same-engine" }
	two.d.EngineRevision = one.d.EngineRevision
	if one.definitionOf(step) == two.definitionOf(step) {
		t.Fatal("step receipts do not bind the selected workflow")
	}
}

func TestMissingWorkflowFailsBeforeAWorkRunIsOpened(t *testing.T) {
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, "missingWorkflow"))
	dh.integ.Configure(func(d *Deps) {
		d.LoadWorkflow = func(name string) (*automations.Automation, error) {
			return nil, fmt.Errorf("%s is not installed", name)
		}
	})
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.RefusalCode != pipelines.CodeStageInvalid || got.WorkRunID != "" {
		t.Fatalf("missing workflow: %+v", got)
	}
}

// The event-to-mode table (decision 3 of the pipelines-seam plan; design
// record D5, plus `release` from the documentation program's D15). A pull
// request runs what it affects; everything that lands, or ships, runs the
// whole suite once.
func TestModeForIsTheEventTable(t *testing.T) {
	cases := []struct {
		event pipelines.Event
		mode  pipelines.Mode
		ok    bool
	}{
		{pipelines.EventPullRequest, pipelines.ModeAffected, true},
		{pipelines.EventMergeGroup, pipelines.ModeFull, true},
		{pipelines.EventPush, pipelines.ModeFull, true},
		{pipelines.EventRelease, pipelines.ModeFull, true},
		// A re-requested check run is not an event of its own: it re-runs the
		// original run's event, so the table has no row for it.
		{"check_run", "", false},
		{"check_suite", "", false},
		{"pull_request_review", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		mode, err := modeForEvent(context.Background(), c.event)
		ok := err == nil && mode != ""
		if mode != c.mode || ok != c.ok {
			t.Errorf("ModeFor(%q) = (%q, %v), want (%q, %v)", c.event, mode, ok, c.mode, c.ok)
		}
	}
}

func TestVersionIsTheTagForAReleaseAndTheSHAOtherwise(t *testing.T) {
	const sha = "a944ae33e9bcd7b2f5d1e5d74dcd8acc6a0ae9df"
	cases := []struct {
		event pipelines.Event
		tag   string
		want  string
	}{
		{pipelines.EventRelease, "v1.4.0", "v1.4.0"},
		{pipelines.EventPush, "", sha},
		{pipelines.EventMergeGroup, "", sha},
		{pipelines.EventPullRequest, "", sha},
		// A tag on a non-release event is not a version: the event decides.
		{pipelines.EventPush, "v1.4.0", sha},
		// A release whose tag has not resolved still names the commit.
		{pipelines.EventRelease, "", sha},
	}
	for _, c := range cases {
		if got, err := versionForEvent(context.Background(), c.event, sha, c.tag); err != nil || got != c.want {
			t.Errorf("Version(%q, sha, %q) = %q, want %q", c.event, c.tag, got, c.want)
		}
	}
}

func TestStageApplicabilityComesFromPinnedPipelineDSL(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	stages := []pipelines.StageSpec{
		{Name: "always"},
		{Name: "pull-request", On: []string{"pull_request"}},
		{Name: "full", On: []string{"full"}},
		{Name: "release", On: []string{"release"}},
	}
	cases := []struct {
		event pipelines.Event
		mode  pipelines.Mode
		want  []string
	}{
		{pipelines.EventPullRequest, pipelines.ModeAffected, []string{"always", "pull-request"}},
		{pipelines.EventPush, pipelines.ModeFull, []string{"always", "full"}},
		{pipelines.EventRelease, pipelines.ModeFull, []string{"always", "full", "release"}},
	}
	for _, tc := range cases {
		got, err := dr.selectStages(context.Background(), stages, tc.event, tc.mode)
		if err != nil || !slices.Equal(got.Included, tc.want) {
			t.Errorf("selectStages(%s/%s) = %v, %v; want %v", tc.event, tc.mode, got.Included, err, tc.want)
		}
	}
	if _, pinned := dr.workflow.definitions["pipelineStageIncluded"]; !pinned {
		t.Fatal("stage applicability definition is not part of the pinned workflow")
	}
}

func TestChangedStagePolicyChangesPipelineWorkflowIdentity(t *testing.T) {
	base := &runDriver{}
	if err := base.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	changedPolicy := workflowSource(t, `@template automation pipelineStageIncluded {
 args { event string! mode string! on []string! }
 return false
}`)
	changed := &runDriver{d: Deps{LoadWorkflow: workflowLoader(changedPolicy)}}
	if err := changed.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	if base.workflowIdentity() == changed.workflowIdentity() {
		t.Fatal("a changed stage selector retained the same run workflow identity")
	}
}

func TestPackageCoverageComesFromPinnedPipelineDSL(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mode   pipelines.Mode
		facts  pipelines.SelectionFacts
		want   bool
		reason string
	}{
		{name: "full mode", mode: pipelines.ModeFull, want: true, reason: "full run mode"},
		{name: "unknown comparison", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Paths: []pipelines.ChangedPathFacts{{Path: "docs/readme.md", BaseName: "readme.md", RepositoryPath: true}},
		}, want: true, reason: "comparison"},
		{name: "empty comparison", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, GraphComplete: true,
		}, want: true, reason: "empty"},
		{name: "incomplete graph", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: false,
			Paths: []pipelines.ChangedPathFacts{{Path: "docs/readme.md", BaseName: "readme.md", RepositoryPath: true}},
		}, want: true, reason: "graph"},
		{name: "unreadable path", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: true,
			Paths: []pipelines.ChangedPathFacts{{Path: "../outside.go"}},
		}, want: true, reason: "repository-relative"},
		{name: "module file at any depth", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: true,
			Paths: []pipelines.ChangedPathFacts{{Path: "tools/go.sum", BaseName: "go.sum", RepositoryPath: true}},
		}, want: true, reason: "module graph"},
		{name: "root pipeline manifest", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: true,
			Paths: []pipelines.ChangedPathFacts{{Path: "memql-package.yaml", BaseName: "memql-package.yaml", RepositoryPath: true}},
		}, want: true, reason: "manifest"},
		{name: "root manifest name in a subdirectory", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: true,
			Paths: []pipelines.ChangedPathFacts{{Path: "tools/memql-package.yaml", BaseName: "memql-package.yaml", RepositoryPath: true}},
		}, want: false},
		{name: "vendored code", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: true,
			Paths: []pipelines.ChangedPathFacts{{Path: "vendor/lib/lib.go", BaseName: "lib.go", RepositoryPath: true, Vendored: true}},
		}, want: true, reason: "vendored"},
		{name: "configured full path", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: true,
			Paths: []pipelines.ChangedPathFacts{{Path: "docs/readme.md", BaseName: "readme.md", RepositoryPath: true, ConfiguredFull: true}},
		}, want: true, reason: "configured full"},
		{name: "narrow affected path", mode: pipelines.ModeAffected, facts: pipelines.SelectionFacts{
			Known: true, ChangedCount: 1, GraphComplete: true,
			Paths: []pipelines.ChangedPathFacts{{Path: "component/memql/engine.go", BaseName: "engine.go", RepositoryPath: true}},
		}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dr.selectPackageCoverage(context.Background(), tc.facts, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			if got.Full != tc.want {
				t.Fatalf("selection = %+v, want full=%v", got, tc.want)
			}
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason %q does not mention %q", got.Reason, tc.reason)
			}
		})
	}
	if _, pinned := dr.workflow.definitions["pipelinePackageSelection"]; !pinned {
		t.Fatal("package coverage policy is not part of the pinned workflow")
	}
}

func TestPerStepPackagePolicyComesFromPinnedPipelineDSL(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		mode     pipelines.Mode
		packages string
		full     bool
		only     string
		want     pipelines.PackagePolicy
	}{
		{name: "full run", mode: pipelines.ModeFull, packages: pipelines.PackagesAffected, want: pipelines.PackagePolicy{Coverage: pipelines.PackageCoverageAll, Filter: pipelines.PackageFilterAll}},
		{name: "all declaration", mode: pipelines.ModeAffected, packages: pipelines.PackagesAll, only: pipelines.OnlyDBGated, want: pipelines.PackagePolicy{Coverage: pipelines.PackageCoverageAll, Filter: pipelines.PackageFilterDBGated}},
		{name: "full graph decision", mode: pipelines.ModeAffected, packages: pipelines.PackagesAffected, full: true, only: pipelines.OnlyNotDBGated, want: pipelines.PackagePolicy{Coverage: pipelines.PackageCoverageAll, Filter: pipelines.PackageFilterNotDBGated}},
		{name: "affected packages", mode: pipelines.ModeAffected, packages: pipelines.PackagesAffected, want: pipelines.PackagePolicy{Coverage: pipelines.PackageCoverageAffected, Filter: pipelines.PackageFilterAll}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected, err := dr.selectPackagePolicies(context.Background(), &pipelines.Spec{Stages: []pipelines.StageSpec{{
				Name: "tests", Steps: []pipelines.StepSpec{{Name: "unit", Packages: tc.packages, Only: tc.only}},
			}}}, tc.mode, pipelines.Selection{Full: tc.full})
			if err != nil {
				t.Fatal(err)
			}
			if got := selected["tests.unit"]; got != tc.want {
				t.Fatalf("per-step policy = %+v, want %+v", got, tc.want)
			}
		})
	}
	if _, pinned := dr.workflow.definitions["pipelinePackageStepSelection"]; !pinned {
		t.Fatal("per-step package policy is not part of the pinned workflow")
	}
}

func TestPinnedPackagePolicyIsAppliedToImportGraphMechanically(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	spec := &pipelines.Spec{
		Select: &pipelines.Select{Go: pipelines.SelectImportGraph, DBGated: []string{"component/database"}},
		Stages: []pipelines.StageSpec{{Name: "tests", Steps: []pipelines.StepSpec{{
			Name: "db", Run: "go test $MEMQL_PACKAGES", Packages: pipelines.PackagesAffected, Only: pipelines.OnlyDBGated,
		}}}},
	}
	graph, err := pipelines.ScanGoTree(fstest.MapFS{
		"go.mod":                         {Data: []byte("module example.test/repo\n")},
		"component/database/database.go": {Data: []byte("package database\n")},
		"component/app/app.go":           {Data: []byte("package app\nimport _ \"example.test/repo/component/database\"\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := pipelines.AnalyzeSelection(graph, []string{"component/database/database.go"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := dr.selectPackageCoverage(context.Background(), facts, pipelines.ModeAffected)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := pipelines.ResolveSelection(graph, facts, coverage)
	if err != nil {
		t.Fatal(err)
	}
	policies, err := dr.selectPackagePolicies(context.Background(), spec, pipelines.ModeAffected, selection)
	if err != nil {
		t.Fatal(err)
	}
	plan, refusal := pipelines.Compile(spec, pipelines.CompileInput{
		Mode: pipelines.ModeAffected, Selector: pipelines.GraphSelector(graph, selection), PackagePolicies: policies,
	})
	if refusal != nil {
		t.Fatal(refusal)
	}
	if got, want := plan.Steps()[0].Packages, []string{"example.test/repo/component/database"}; !slices.Equal(got, want) {
		t.Fatalf("compiled package list = %v, want %v", got, want)
	}
}

func TestChangedBucketPolicyComesFromPinnedPipelineDSL(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	spec := &pipelines.Spec{Select: &pipelines.Select{Buckets: map[string][]string{"docs": {"docs/**"}}}}
	cases := []struct {
		name    string
		mode    pipelines.Mode
		changed []string
		known   bool
		full    bool
		want    bool
	}{
		{name: "full run", mode: pipelines.ModeFull, want: true},
		{name: "unknown compare", mode: pipelines.ModeAffected, changed: []string{"docs/readme.md"}, want: true},
		{name: "empty compare", mode: pipelines.ModeAffected, known: true, want: true},
		{name: "unsafe path", mode: pipelines.ModeAffected, changed: []string{"../outside"}, known: true, want: true},
		{name: "matched path", mode: pipelines.ModeAffected, changed: []string{"docs/readme.md"}, known: true, want: true},
		{name: "unmatched path", mode: pipelines.ModeAffected, changed: []string{"component/memql/engine.go"}, known: true, want: false},
		{name: "full package selection", mode: pipelines.ModeAffected, changed: []string{"memql-package.yaml"}, known: true, full: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dr.selectBuckets(context.Background(), spec, tc.mode, tc.changed, tc.known, tc.full)
			if err != nil {
				t.Fatal(err)
			}
			included := slices.Contains(got.Included, "docs")
			if included != tc.want {
				t.Fatalf("included buckets = %v, want docs included=%v", got.Included, tc.want)
			}
		})
	}
	if _, pinned := dr.workflow.definitions["pipelineBucketIncluded"]; !pinned {
		t.Fatal("bucket applicability policy is not part of the pinned workflow")
	}
}

func TestBucketOnlyPipelineUsesFullCoverageEscalation(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	spec := &pipelines.Spec{
		Select: &pipelines.Select{Buckets: map[string][]string{"docs": {"docs/**"}}},
		Stages: []pipelines.StageSpec{{Name: "checks", Steps: []pipelines.StepSpec{{
			Name: "docs", Run: "make docs-checks", When: &pipelines.When{Bucket: "docs"},
		}}}},
	}
	for _, tc := range []struct {
		name    string
		changed []string
		wantRun bool
	}{
		{name: "manifest change escalates", changed: []string{"memql-package.yaml"}, wantRun: true},
		{name: "module graph change escalates", changed: []string{"go.mod"}, wantRun: true},
		{name: "matching bucket runs", changed: []string{"docs/guide.md"}, wantRun: true},
		{name: "unmatched path stays narrow", changed: []string{"component/memql/engine.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := pipelines.AnalyzeChangedPaths(tc.changed, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			coverage, err := dr.selectPackageCoverage(context.Background(), facts, pipelines.ModeAffected)
			if err != nil {
				t.Fatal(err)
			}
			buckets, err := dr.selectBuckets(context.Background(), spec, pipelines.ModeAffected, tc.changed, true, coverage.Full)
			if err != nil {
				t.Fatal(err)
			}
			plan, refusal := pipelines.Compile(spec, pipelines.CompileInput{
				Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, Compute: pipelines.ComputeCluster,
				BucketSelection: &buckets, StageSelection: &pipelines.StageSelection{Included: []string{"checks"}},
			})
			if refusal != nil {
				t.Fatal(refusal)
			}
			if got := findPlanStep(t, plan, "checks.docs").Skip == nil; got != tc.wantRun {
				t.Errorf("bucket-only docs step run = %v, want %v (full=%v)", got, tc.wantRun, coverage.Full)
			}
		})
	}
}

func TestChangedBucketPolicyChangesPipelineWorkflowIdentity(t *testing.T) {
	base := &runDriver{}
	if err := base.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	changedPolicy := workflowSource(t, `@template automation pipelineBucketIncluded {
 args { mode string! known bool! changedCount int! selectionFull bool! pathsValid bool! matched bool! }
 return args.matched
}`)
	changed := &runDriver{d: Deps{LoadWorkflow: workflowLoader(changedPolicy)}}
	if err := changed.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	if base.workflowIdentity() == changed.workflowIdentity() {
		t.Fatal("a changed bucket selector retained the same run workflow identity")
	}
}

func TestFullPackageSelectionRunsEveryBucketedStep(t *testing.T) {
	dr := &runDriver{}
	if err := dr.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	graph, err := pipelines.ScanGoTree(fstest.MapFS{
		"go.mod":                {Data: []byte("module example.test/repo\n")},
		"component/engine/a.go": {Data: []byte("package engine\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := &pipelines.Spec{
		Image: "example.test/toolchain@sha256:...",
		Select: &pipelines.Select{Go: pipelines.SelectImportGraph, Buckets: map[string][]string{
			"docs": {"docs/**"}, "os": {"clients/**"},
		}},
		Stages: []pipelines.StageSpec{{Name: "checks", Steps: []pipelines.StepSpec{
			{Name: "os", Run: "make os-checks", When: &pipelines.When{Bucket: "os"}},
			{Name: "docs", Run: "make docs-checks", When: &pipelines.When{Bucket: "docs"}},
			{Name: "packages", Run: "go test $MEMQL_PACKAGES", Packages: pipelines.PackagesAffected},
		}}},
	}

	cases := []struct {
		name    string
		changed []string
		wantOS  bool
		wantDoc bool
	}{
		{name: "manifest change escalates to full", changed: []string{"memql-package.yaml"}, wantOS: true, wantDoc: true},
		{name: "single bucket change stays narrow", changed: []string{"clients/os/style.css"}, wantOS: true, wantDoc: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := pipelines.AnalyzeSelection(graph, tc.changed, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := dr.selectPackageCoverage(context.Background(), facts, pipelines.ModeAffected)
			if err != nil {
				t.Fatal(err)
			}
			selection, err := pipelines.ResolveSelection(graph, facts, decision)
			if err != nil {
				t.Fatal(err)
			}
			buckets, err := dr.selectBuckets(context.Background(), spec, pipelines.ModeAffected, tc.changed, true, selection.Full)
			if err != nil {
				t.Fatal(err)
			}
			policies, err := dr.selectPackagePolicies(context.Background(), spec, pipelines.ModeAffected, selection)
			if err != nil {
				t.Fatal(err)
			}
			plan, refusal := pipelines.Compile(spec, pipelines.CompileInput{
				Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, Compute: pipelines.ComputeCluster,
				Selector: pipelines.GraphSelector(graph, selection), PackagePolicies: policies,
				BucketSelection: &buckets, StageSelection: &pipelines.StageSelection{Included: []string{"checks"}},
			})
			if refusal != nil {
				t.Fatal(refusal)
			}
			for key, wantRun := range map[string]bool{"checks.os": tc.wantOS, "checks.docs": tc.wantDoc} {
				step := findPlanStep(t, plan, key)
				if gotRun := step.Skip == nil; gotRun != wantRun {
					t.Errorf("%s skipped=%v, want run=%v", key, !gotRun, wantRun)
				}
			}
		})
	}
}

func findPlanStep(t *testing.T, plan pipelines.Plan, key string) pipelines.Step {
	t.Helper()
	for _, step := range plan.Steps() {
		if step.Key == key {
			return step
		}
	}
	t.Fatalf("plan has no step %q", key)
	return pipelines.Step{}
}

func TestChangedSelectionPolicyChangesPipelineWorkflowIdentity(t *testing.T) {
	base := &runDriver{}
	if err := base.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	changedPolicy := workflowSource(t, `@template automation pipelinePackageSelection {
 args { mode string! known bool! graphComplete bool! changed []any! }
 return {full: false, reason: "changed policy"}
}`)
	changed := &runDriver{d: Deps{LoadWorkflow: workflowLoader(changedPolicy)}}
	if err := changed.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	if base.workflowIdentity() == changed.workflowIdentity() {
		t.Fatal("a changed package selector retained the same run workflow identity")
	}
}

func TestChangedPerStepPackagePolicyChangesPipelineWorkflowIdentity(t *testing.T) {
	base := &runDriver{}
	if err := base.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	changedPolicy := workflowSource(t, `@template automation pipelinePackageStepSelection {
 args { mode string! packages string! selectionFull bool! only string! }
 return {coverage: "all", filter: "all"}
}`)
	changed := &runDriver{d: Deps{LoadWorkflow: workflowLoader(changedPolicy)}}
	if err := changed.prepareWorkflow(""); err != nil {
		t.Fatal(err)
	}
	if base.workflowIdentity() == changed.workflowIdentity() {
		t.Fatal("a changed per-step package policy retained the same run workflow identity")
	}
}

func TestEmptyStageSelectionCannotEraseExistingWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  Run
		want string
	}{
		{name: "fresh run", run: Run{}, want: ConclusionSuccess},
		{name: "existing journal", run: Run{WorkRunID: "work-1"}, want: ConclusionFailure},
		{name: "failed-only rerun", run: Run{RerunFailedOnly: true}, want: ConclusionFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dr := &runDriver{run: tc.run}
			if got := dr.emptyPlanVerdict().conclusion; got != tc.want {
				t.Fatalf("empty plan conclusion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPipelineWorkflowCannotReturnEarlyAndPass(t *testing.T) {
	a := workflowSource(t, `@template automation emptyPipelineWorkflow { args { stages []any! } return true }`)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || len(dh.exec.sentKeys()) != 0 {
		t.Fatalf("unfinished workflow passed: %+v", got)
	}
}

func TestPipelineWorkflowCannotSkipEveryRequiredStepAndPass(t *testing.T) {
	a := workflowSource(t, `@template
automation fabricatedPassingWorkflow {
 args { stages []any! }
 for stage in args.stages {
  for item in stage.steps {
   builtin pipelineSkipStep(stepKey: item.key, code: "pipeline_stage_blocked", reason: "fabricated")
  }
 }
}`)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Conclusion != ConclusionFailure || len(dh.exec.sentKeys()) != 0 {
		t.Fatalf("fabricated skips produced a passing check: %+v", got)
	}
}

func TestPipelineWorkflowFreezesResolvedActionDefinitions(t *testing.T) {
	name := fmt.Sprintf("boundaryFrozenAction%d", time.Now().UnixNano())
	initial := &actions.Action{Name: name, Version: 1, Enabled: true,
		Capability: "integration.pipelines.executeStep", Kind: "primitive",
		Params:   []actions.Param{{Name: "stepKey", Type: "string", Required: true}},
		CallArgs: []actions.CallArg{{Key: "stepKey", ArgPath: "stepKey"}}}
	if err := actions.Default().Register(initial); err != nil {
		t.Fatal(err)
	}
	a := workflowSource(t, fmt.Sprintf(`@template automation frozenActionWorkflow {
 args { stages []any! }
 action %s(stepKey: "checks.vet")
}`, name))
	dr := &runDriver{d: Deps{LoadWorkflow: workflowLoader(a)}}
	if err := dr.prepareWorkflow(a.Name); err != nil {
		t.Fatal(err)
	}
	fingerprint := dr.workflowIdentity()
	if err := actions.Default().Register(&actions.Action{Name: name, Version: 2, Enabled: true,
		Capability: "integration.pipelines.reportProgress"}); err != nil {
		t.Fatal(err)
	}
	initial.Capability = "integration.pipelines.reportProgress"
	initial.Params[0].Name = "changed"
	initial.CallArgs[0].ArgPath = "changed"
	frozen, ok := dr.workflow.actions[name].LookupLatest(name)
	if !ok || frozen.Version != 1 || frozen.Capability != "integration.pipelines.executeStep" ||
		frozen.Params[0].Name != "stepKey" || frozen.CallArgs[0].ArgPath != "stepKey" || dr.workflowIdentity() != fingerprint {
		t.Fatalf("prepared workflow changed under its recorded identity: %+v", frozen)
	}
}
