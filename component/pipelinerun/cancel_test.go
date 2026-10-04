package pipelinerun

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

// cancel_test.go -- asking a run to stop.

func queuedRun(p Pipeline, sha string) Run {
	key := pipelines.RunKey(p.Repository, sha, pipelines.ModeAffected, pipelines.EventPullRequest)
	return Run{
		ID: RunIDFor(p.ID, key, 1), OwnerUserID: p.OwnerUserID, PipelineID: p.ID, Repository: p.Repository, SHA: sha,
		Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest, RunKey: key, Attempt: 1, Trigger: TriggerWebhook,
		PullRequest: 42, Status: StatusQueued, CheckRunID: 501, CheckRunState: CheckRunWritten, QueuedAt: testNow.Add(-tenMinutes),
	}
}

// A queued run nobody drives has nothing in flight and nobody to conclude it:
// the cancel concludes it, check run included.
func TestCancellingAQueuedRunNobodyDrivesConcludesItOnTheSpot(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	run := queuedRun(p, shaA)
	h.store.addRun(run)

	nodes, err := h.integ.handleCancel(personCtx(ownerID), map[string]any{"runId": run.ID}, 0)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ := h.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled {
		t.Errorf("status %q conclusion %q; want completed and cancelled", got.Status, got.Conclusion)
	}
	if !got.CancelRequested || got.CancelledBy != ownerID || got.FinishedAt.IsZero() {
		t.Errorf("the ask and who asked are recorded: %+v", got)
	}
	updates := h.github.updatedRuns()
	if len(updates) != 1 || updates[0].ID != 501 {
		t.Fatalf("the check run is moved: %+v", updates)
	}
	if cr := updates[0].Run; cr.Status != "completed" || cr.Conclusion != "cancelled" || cr.Output == nil || cr.Output.Title != "Cancelled" {
		t.Errorf("check run = %+v", cr)
	}
	payload := decodeNode(t, nodes)
	if payload["status"] != StatusCompleted || payload["conclusion"] != ConclusionCancelled {
		t.Errorf("answer = %v", payload)
	}
	keys := h.gate.seen()
	if len(keys) != 1 || keys[0] != RunGateKey(run.ID) {
		t.Errorf("the cancel is decided under the run's gate: %v", keys)
	}
}

// A run a driver holds is only FLAGGED; the driver records the outcome. When
// this node is the driver, the hook tells it now.
func TestCancellingADrivenRunFlagsItAndSignalsTheLocalDriver(t *testing.T) {
	for name, c := range map[string]struct {
		driver     string
		wantSignal bool
	}{
		"driven here":      {"agent-a", true},
		"driven elsewhere": {"agent-b", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			run := queuedRun(p, shaA)
			run.Status, run.DriverNodeID = StatusInProgress, c.driver
			h.store.addRun(run)
			var signalled []string
			h.integ.Configure(func(d *Deps) {
				d.SignalCancel = func(_ context.Context, id string) { signalled = append(signalled, id) }
			})

			if err := h.integ.RequestCancel(context.Background(), run.ID, "substrate"); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			got, _ := h.store.run(run.ID)
			if got.Status != StatusInProgress || got.Conclusion != "" {
				t.Errorf("a driven run is not concluded by the ask: %s/%s", got.Status, got.Conclusion)
			}
			if !got.CancelRequested || got.CancelledBy != "substrate" {
				t.Errorf("flag = %v by %q", got.CancelRequested, got.CancelledBy)
			}
			if (len(signalled) == 1) != c.wantSignal {
				t.Errorf("signalled %v, want signal=%v", signalled, c.wantSignal)
			}
			if n := len(h.github.updatedRuns()); n != 0 {
				t.Errorf("the driver moves the check run, not the ask: %d updates", n)
			}
		})
	}
}

func TestCancelRefusesAFinishedRunAndSomebodyElsesRun(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	run := queuedRun(p, shaA)
	h.store.addRun(run)

	if _, err := h.integ.handleCancel(personCtx(otherID), map[string]any{"runId": run.ID}, 0); err == nil {
		t.Errorf("another person cancelled somebody else's run")
	}
	if got, _ := h.store.run(run.ID); got.CancelRequested {
		t.Errorf("a refused cancel wrote the flag")
	}

	run.Status, run.Conclusion = StatusCompleted, ConclusionSuccess
	h.store.addRun(run)
	if err := h.integ.RequestCancel(context.Background(), run.ID, ownerID); !errors.Is(err, ErrRunFinished) {
		t.Errorf("err = %v, want ErrRunFinished", err)
	}
	if err := h.integ.RequestCancel(context.Background(), "no-such-run", ownerID); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

func TestCancelWithA403StillConcludesAndRecordsTheNote(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	run := queuedRun(p, shaA)
	h.store.addRun(run)
	h.github.updateErr = errStatus(403)

	if err := h.integ.RequestCancel(context.Background(), run.ID, ownerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _ := h.store.run(run.ID)
	if got.Conclusion != ConclusionCancelled || got.CheckRunState != CheckRunRefused {
		t.Errorf("conclusion %q checkRunState %q", got.Conclusion, got.CheckRunState)
	}
	if len(got.Notes) != 1 || got.Notes[0].Code != pipelines.CodeCheckPermission {
		t.Errorf("notes = %+v", got.Notes)
	}
}

// The check run of a run a cancel concluded is written AFTER the run's gate
// is released (the fakes fail a GitHub call under it). When that final write
// does not land, the row must not keep saying `written` over a check run that
// still shows the run queued: it records `unavailable`, which recovery
// republishes.
func TestACancelsFinalCheckRunThatDidNotLandIsRecordedUnavailable(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	run := queuedRun(p, shaA)
	h.store.addRun(run)
	h.github.updateErr = errStatus(502)

	if err := h.integ.RequestCancel(context.Background(), run.ID, ownerID); err != nil {
		t.Fatalf("a check run that did not move does not fail the cancel: %v", err)
	}
	got, _ := h.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled {
		t.Errorf("the run is concluded whatever GitHub answered: %s/%s", got.Status, got.Conclusion)
	}
	if got.CheckRunState != CheckRunUnavailable || got.CheckRunID != 501 {
		t.Errorf("checkRunState %q id %d; want unavailable, the check run kept", got.CheckRunState, got.CheckRunID)
	}
	if keys := h.gate.seen(); len(keys) != 2 || keys[0] != RunGateKey(run.ID) || keys[1] != RunGateKey(run.ID) {
		t.Errorf("gate keys = %v; want the run's gate for the conclusion, then again, after GitHub, to record the answer", keys)
	}
}
