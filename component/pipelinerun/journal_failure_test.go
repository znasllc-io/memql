package pipelinerun

import (
	"context"
	"errors"
	"github.com/znasllc-io/memql/component/pipelines"
	"testing"
	"time"
)

func TestJournalFailureCannotStartUnrecordedWorkOrPublishSuccess(t *testing.T) {
	for _, tc := range []struct {
		mutation string
		maxSteps int
	}{
		{"createWorkStep", 0}, // no durable intent, so no command
		{"updateWorkStep", 1}, // no receipt, so no dependent stage
		{"updateWorkRun", 3},  // no terminal record, so no check conclusion
		{"updateWorkGoal", 3},
	} {
		t.Run(tc.mutation, func(t *testing.T) {
			dh := newDriveHarness(t, driveManifest)
			dh.work.refuseCall, dh.work.refuseErr = tc.mutation, errors.New("journal unavailable")
			dh.exec.onAcknowledge = func(req pipelines.StepRequest) {
				if rows := dh.work.receiptsOf(req.StepKey); len(rows) == 0 {
					t.Errorf("acknowledged %s without its durable receipt", req.StepKey)
				}
				if len(req.Secrets) != 0 {
					t.Error("cleanup retained resolved secrets")
				}
			}
			run := dh.openRun(t, prOpening())
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			if got.Status == StatusCompleted || got.Conclusion != "" {
				t.Fatalf("a journal failure concluded the run: %+v", got)
			}
			if sent := dh.exec.sentKeys(); len(sent) > tc.maxSteps {
				t.Fatalf("work advanced without its record: %v", sent)
			}
			if (tc.mutation == "createWorkStep" || tc.mutation == "updateWorkStep") && len(dh.exec.acknowledged()) != 0 {
				t.Fatal("a failed receipt discarded recoverable executor evidence")
			}
			if last := dh.lastUpdate(t).Run; last.Conclusion == "success" {
				t.Fatal("GitHub was told success without durable evidence")
			}
			// A recovered writer can finish the same pinned run; an outage
			// must not leave a false terminal row that prevents recovery.
			dh.work.mu.Lock()
			dh.work.refuseCall, dh.work.refuseErr = "", nil
			dh.work.mu.Unlock()
			dh.integ.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(time.Minute) } })
			if err := dh.integ.RecoverRuns(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitDrives(t, dh.integ)
			got, _ = dh.store.run(run.ID)
			if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess {
				t.Fatalf("recovery after the journal returned: %+v", got)
			}
		})
	}
}
