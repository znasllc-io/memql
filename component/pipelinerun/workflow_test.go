package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

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
