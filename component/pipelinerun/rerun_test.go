package pipelinerun

import (
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// rerun_test.go -- a person's re-run (decision 3).

func TestRerunOpensTheNextAttemptWithTheOriginalsModeAndEvent(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	original := queuedRun(p, shaA)
	original.Status, original.Conclusion = StatusCompleted, ConclusionFailure
	original.Version = shaA
	h.store.addRun(original)

	nodes, err := h.integ.handleRerun(personCtx(ownerID), map[string]any{"runId": original.ID}, 0)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	payload := decodeNode(t, nodes)
	next, ok := h.store.run(payload["runId"].(string))
	if !ok {
		t.Fatalf("no run %v", payload["runId"])
	}
	if next.Attempt != 2 || next.Mode != original.Mode || next.Event != original.Event || next.RunKey != original.RunKey {
		t.Errorf("next = attempt %d mode %s event %s key %q", next.Attempt, next.Mode, next.Event, next.RunKey)
	}
	if next.Trigger != TriggerRerun || next.RerunOf != original.ID || next.Status != StatusQueued {
		t.Errorf("trigger %q rerunOf %q status %q", next.Trigger, next.RerunOf, next.Status)
	}
	if next.PullRequest != original.PullRequest || next.Version != original.Version {
		t.Errorf("the re-run carries the original's facts: %+v", next)
	}
	if n := len(h.github.createdRuns()); n != 1 {
		t.Errorf("the re-run gets its own check run: %d", n)
	}
	if orig, _ := h.store.run(original.ID); orig.Conclusion != ConclusionFailure {
		t.Errorf("the original stays as it ended")
	}
}

func TestRerunRefusals(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	going := queuedRun(p, shaA)
	h.store.addRun(going)

	// Somebody else's run is no run.
	if _, err := h.integ.Rerun(personCtx(otherID), going.ID, false); err == nil {
		t.Errorf("another person re-ran somebody else's run")
	}
	// A run still going is not re-run.
	if _, err := h.integ.Rerun(personCtx(ownerID), going.ID, false); !errors.Is(err, ErrRunInProgress) {
		t.Errorf("err = %v, want ErrRunInProgress", err)
	}

	// A fork stays a fork.
	fork := queuedRun(p, shaB)
	fork.Status, fork.Conclusion, fork.RefusalCode = StatusCompleted, ConclusionRefused, pipelines.CodeForkRefused
	h.store.addRun(fork)
	if _, err := h.integ.Rerun(personCtx(ownerID), fork.ID, false); refusalCode(err) != pipelines.CodeForkRefused {
		t.Errorf("fork re-run: %v", err)
	}

	// A disconnected pipeline opens no runs.
	done := queuedRun(p, shaC)
	done.Status, done.Conclusion = StatusCompleted, ConclusionSuccess
	h.store.addRun(done)
	p.Status = PipelineDisconnected
	h.store.addPipeline(p)
	if _, err := h.integ.Rerun(personCtx(ownerID), done.ID, false); refusalCode(err) != pipelines.CodeDisconnected {
		t.Errorf("disconnected re-run: %v", err)
	}
	if n := len(h.store.runCreates); n != 0 {
		t.Errorf("no refused re-run wrote a run: %d", n)
	}
}

// TestARerunOfFailedStepsIsRefusedWhenNothingFailed: the act is offered only
// on a run with something to run again, and the engine holds the same line.
// A run that passed, and a run refused before any step (a manifest that does
// not compile), have no failed step; the second is still re-runnable whole.
func TestARerunOfFailedStepsIsRefusedWhenNothingFailed(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)

	passedRun := queuedRun(p, shaA)
	passedRun.Status, passedRun.Conclusion = StatusCompleted, ConclusionSuccess
	h.store.addRun(passedRun)
	if _, err := h.integ.Rerun(personCtx(ownerID), passedRun.ID, true); refusalCode(err) != pipelines.CodeNothingToRerun {
		t.Errorf("a passed run has no failed step to re-run: %v", err)
	}

	refused := queuedRun(p, shaB)
	refused.Status, refused.Conclusion, refused.RefusalCode = StatusCompleted, ConclusionFailure, pipelines.CodeStepInvalid
	h.store.addRun(refused)
	if _, err := h.integ.Rerun(personCtx(ownerID), refused.ID, true); refusalCode(err) != pipelines.CodeNothingToRerun {
		t.Errorf("a run refused before any step has no failed step to re-run: %v", err)
	}
	if n := len(h.store.runCreates); n != 0 {
		t.Fatalf("no refused re-run wrote a run: %d", n)
	}
	whole, err := h.integ.Rerun(personCtx(ownerID), refused.ID, false)
	if err != nil || whole.RerunFailedOnly {
		t.Errorf("the refused run is still re-run whole: %+v %v", whole, err)
	}
}
