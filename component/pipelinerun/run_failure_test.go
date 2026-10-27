package pipelinerun

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// run_failure_test.go -- a run that fails for a reason no step reported says
// why on its row, on its check run and, because the row is where the reason
// lives, on a check run recovery republishes from the row alone.
//
// The case is memql pull requests 6312, 6313 and 6318 (2026-10-27): every
// stage passed, the workflow then failed reaching a model for the independent
// review, and the required check read "Failed" over a table of Passed with the
// reason only in the Nexus work run.

// reviewAfterStages runs the default stages, then reports the merge group's
// review: the workflow step that fails once every stage has passed.
const reviewAfterStages = `use pipelines.automations.{ runPipelineStages }
@template
automation reviewAfterStages {
 args {stages []any!}
 automation runPipelineStages(stages: args.stages)
 builtin pipelineReportMergeGroupReview()
}`

// noModelError is the review's failure. It echoes the run's one secret value,
// so the test can see that the reason is masked wherever it is written.
var noModelError = fmt.Errorf("every_door_shut: no door to a model is open for this call (asked as %s)", shopSecret)

func mergeGroupOpening() Opening {
	o := prOpening()
	o.Event, o.Branch, o.PullRequest = pipelines.EventMergeGroup, "gh-readonly-queue/main/opaque", 0
	return o
}

// driveFailingWorkflow drives a merge-group run whose workflow fails after
// every step passed, through gh, and answers the concluded row.
func driveFailingWorkflow(t *testing.T, configure func(dh *driveHarness, d *Deps)) (*driveHarness, Run) {
	t.Helper()
	a := workflowSource(t, reviewAfterStages)
	dh := newDriveHarness(t, manifestWorkflow(driveManifest, a.Name))
	dh.integ.Configure(func(d *Deps) {
		bindTestWorkflow(d, a)
		d.ReportMergeGroupReview = func(context.Context, Pipeline, Run, func(context.Context) error) (map[string]any, error) {
			return nil, noModelError
		}
		if configure != nil {
			configure(dh, d)
		}
	})
	run := dh.openRun(t, mergeGroupOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionFailure {
		t.Fatalf("the run ends completed and failed: %s/%s\n%s", got.Status, got.Conclusion, dh.logs.String())
	}
	for _, st := range got.Stages {
		if st.Status != StagePassed {
			t.Fatalf("every stage passed before the workflow failed: %+v", got.Stages)
		}
	}
	return dh, got
}

// assertStatesTheReason fails unless a completed check run's title and summary
// state the workflow's reason, masked, beside a stage table that reads Passed.
func assertStatesTheReason(t *testing.T, w checkWrite) {
	t.Helper()
	out := w.Run.Output
	if w.Run.Status != StatusCompleted || w.Run.Conclusion != "failure" || out == nil {
		t.Fatalf("a completed, failed check run: %s %s %+v", w.Run.Status, w.Run.Conclusion, out)
	}
	if !strings.HasPrefix(out.Title, "Failed: ") || !strings.Contains(out.Title, "every_door_shut: no door to a model is open for this call") {
		t.Errorf("the title states the reason: %q", out.Title)
	}
	for _, want := range []string{
		"| checks | Passed | 1 passed | 42s |",
		"| tests | Passed | 2 passed | 42s |",
		"every\\_door\\_shut: no door to a model is open for this call",
		"`" + pipelines.CodeExecutorError + "`",
	} {
		if !strings.Contains(out.Summary, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, out.Summary)
		}
	}
	for name, field := range map[string]string{"title": out.Title, "summary": out.Summary, "text": out.Text} {
		if strings.Contains(field, shopSecret) {
			t.Errorf("the %s carries the secret value: %q", name, field)
		}
	}
}

func TestAWorkflowThatFailsAfterEveryStepPassedSaysWhy(t *testing.T) {
	dh, got := driveFailingWorkflow(t, nil)

	// The row: the run's own reason, masked, and not a refusal -- a refusal
	// would replace the stage table with itself.
	if got.FailureCode != pipelines.CodeExecutorError ||
		!strings.Contains(got.FailureMessage, "every_door_shut: no door to a model is open for this call (asked as ***)") {
		t.Errorf("the row carries the workflow's reason: %q %q", got.FailureCode, got.FailureMessage)
	}
	if strings.Contains(got.FailureMessage, shopSecret) {
		t.Errorf("the row's reason carries the secret value: %q", got.FailureMessage)
	}
	if got.RefusalCode != "" || got.RefusalMessage != "" {
		t.Errorf("a workflow failure is not a refusal: %q %q", got.RefusalCode, got.RefusalMessage)
	}

	// The check run, as the driver concluded it.
	assertStatesTheReason(t, dh.lastUpdate(t))

	// The work run keeps the same reason, as it did before the row had one.
	if wr := dh.work.run(got.WorkRunID); argString(wr, "status") != "failed" ||
		argString(wr, "errorCode") != pipelines.CodeExecutorError || argString(wr, "errorMessage") != got.FailureMessage {
		t.Errorf("the work run closes failed with the row's reason: %v", wr)
	}
}

// The reason survives the drive that found it: a final check run that did not
// land is republished by recovery, possibly on another replica, from the run
// row and its work rows alone -- no verdict is left in any memory by then.
func TestARepublishedCheckRunStillSaysWhyTheRunFailed(t *testing.T) {
	dh, got := driveFailingWorkflow(t, func(dh *driveHarness, d *Deps) {
		// Every completed write is refused, so the final report never lands.
		flaky := &flakyFinal{GitHub: dh.github}
		flaky.fails.Store(99)
		d.GitHub = flaky
	})
	if got.CheckRunState != CheckRunUnavailable {
		t.Fatalf("the final report did not land, and the row says so: %q", got.CheckRunState)
	}
	for _, w := range dh.github.updatedRuns() {
		if w.Run.Status == StatusCompleted {
			t.Fatalf("no completed report reached GitHub before recovery: %+v", w.Run.Output)
		}
	}

	// A replica that never drove the run republishes it, through a GitHub
	// that takes the write.
	peer := dh.peer("agent-b")
	if err := peer.RecoverRuns(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	var final *checkWrite
	for _, w := range dh.github.updatedRuns() {
		if w.Run.Status == StatusCompleted {
			final = &w
		}
	}
	if final == nil {
		t.Fatalf("recovery did not republish the final check run\n%s", dh.logs.String())
	}
	assertStatesTheReason(t, *final)
}

// A resumed run whose plan changed after every step had finished fails no
// step, so the reason it fails for is the run's own. (When steps were still
// unfinished, each fails with the reason and the row restates nothing:
// TestAResumedRunWhosePlanChangedFailsNodeLost.)
func TestADivergedResumeWithNothingLeftToFailSaysWhy(t *testing.T) {
	dr := &runDriver{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	passed := &stepTrack{step: pipelines.Step{Key: "checks.vet", Stage: "checks"}}
	passed.set(StepState{Key: "checks.vet", Stage: "checks", Status: StepSucceeded})
	dr.tracks = []*stepTrack{passed}
	v := dr.diverged(context.Background(), "the execution definition or order changed for step checks.vet")
	if v.conclusion != ConclusionFailure || !v.outsideSteps || v.workCode != pipelines.CodeNodeLost ||
		!strings.Contains(v.workMessage, "the execution definition or order changed for step checks.vet") {
		t.Errorf("verdict = %+v", v)
	}
}
