package pipelinerun

import (
	"context"
	"errors"
	"testing"
	"time"

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

// cancelIfWaiting concludes a run only while nobody drives it -- the one state
// in which nothing is in flight and nobody else would conclude it. A run an
// agent claimed, started or not, is left alone to finish; a finished run keeps
// its answer. Each is decided under the run's gate on a fresh read.
func TestCancelIfWaitingConcludesOnlyARunNobodyDrives(t *testing.T) {
	for name, c := range map[string]struct {
		shape     func(r *Run)
		concluded bool
		err       error
	}{
		"waiting":                  {func(r *Run) {}, true, nil},
		"claimed, not yet started": {func(r *Run) { r.DriverNodeID, r.DriverLeaseID, r.DriverHeartbeatAt = "agent-b", "lease-b", testNow }, false, nil},
		"claimed long ago":         {func(r *Run) { r.DriverNodeID, r.DriverHeartbeatAt = "agent-b", testNow.Add(-time.Hour) }, false, nil},
		"in progress":              {func(r *Run) { *r = drivenBy(*r, "agent-b") }, false, nil},
		"asked to stop already":    {func(r *Run) { r.CancelRequested, r.CancelledBy = true, ownerID }, false, nil},
		"finished":                 {func(r *Run) { r.Status, r.Conclusion = StatusCompleted, ConclusionSuccess }, false, ErrRunFinished},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			logs := withLogs(h.integ)
			p := testPipeline(DeliveryWebhook)
			h.store.addPipeline(p)
			run := queuedRun(p, shaA)
			c.shape(&run)
			h.store.addRun(run)

			got, concluded, err := h.integ.cancelIfWaiting(context.Background(), h.integ.snapshot(), run.ID, supersededPrefix+"run-of-shaC")
			if concluded != c.concluded || !errors.Is(err, c.err) || (c.err == nil && err != nil) {
				t.Fatalf("concluded %v err %v; want %v %v", concluded, err, c.concluded, c.err)
			}
			now, _ := h.store.run(run.ID)
			if got.ID != run.ID || got.Status != now.Status {
				t.Errorf("the answer is the row as it stands: %+v, stored %+v", got, now)
			}
			if keys := h.gate.seen(); len(keys) == 0 || keys[0] != RunGateKey(run.ID) {
				t.Errorf("decided under the run's gate: %v", keys)
			}
			if fresh := len(h.store.runUpdates); c.concluded != (fresh > 0) {
				t.Errorf("writes %d; a run is written only when it is concluded", fresh)
			}
			if !c.concluded {
				if now.Finished() != run.Finished() || now.CancelRequested != run.CancelRequested || now.CancelledBy != run.CancelledBy {
					t.Errorf("a run left alone is untouched: %+v", now)
				}
				if len(h.github.updatedRuns()) != 0 {
					t.Errorf("a run left alone keeps its check run: %+v", h.github.updatedRuns())
				}
				if name != "finished" && name != "asked to stop already" {
					wantLogged(t, logs.String(), "already executing; it finishes", "INFO")
				}
				return
			}
			if now.Status != StatusCompleted || now.Conclusion != ConclusionCancelled || !now.CancelRequested ||
				now.CancelledBy != supersededPrefix+"run-of-shaC" || now.FinishedAt.IsZero() {
				t.Errorf("concluded cancelled, superseded: %+v", now)
			}
			if !stoppedBySupersede(now) {
				t.Errorf("the run reads as stopped by a supersede: %+v", now)
			}
			updates := h.github.updatedRuns()
			if len(updates) != 1 || updates[0].ID != 501 || updates[0].Run.Conclusion != "cancelled" {
				t.Errorf("its check run is concluded cancelled, after the gate: %+v", updates)
			}
		})
	}
}

// The cancel a person asks for and the one a supersede makes write the same
// conclusion for a run nobody drives, through one helper: the request records
// who asked, the conclusion and the time.
func TestAPersonsCancelAndASupersedeConcludeAWaitingRunAlike(t *testing.T) {
	h := newHarness(t)
	p := testPipeline(DeliveryWebhook)
	h.store.addPipeline(p)
	byPerson, bySupersede := queuedRun(p, shaA), queuedRun(p, shaB)
	bySupersede.ID = "run-superseded"
	h.store.addRun(byPerson)
	h.store.addRun(bySupersede)

	if err := h.integ.RequestCancel(context.Background(), byPerson.ID, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.integ.cancelIfWaiting(context.Background(), h.integ.snapshot(), bySupersede.ID, supersededPrefix+"x"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	a, _ := h.store.run(byPerson.ID)
	b, _ := h.store.run(bySupersede.ID)
	if a.Status != b.Status || a.Conclusion != b.Conclusion || a.CancelRequested != b.CancelRequested || !a.FinishedAt.Equal(b.FinishedAt) {
		t.Errorf("the two conclusions differ: %+v\n%+v", a, b)
	}
	if a.CancelledBy != ownerID || b.CancelledBy != supersededPrefix+"x" {
		t.Errorf("each records who asked: %q, %q", a.CancelledBy, b.CancelledBy)
	}
}
