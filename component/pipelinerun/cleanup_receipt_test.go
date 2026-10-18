package pipelinerun

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

func TestCancellationFailureRemainsUnfinishedUntilAnotherDriverConfirms(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.exec.cancelError = errors.New("runner cleanup unavailable")
	release := make(chan struct{})
	defer close(release)
	blockTests(dh, release)
	run := dh.openRun(t, prOpening())
	dh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	dh.awaitEntered(t, "checks.vet", "tests.unit", "tests.lint")
	if err := dh.integ.RequestCancel(context.Background(), run.ID, ownerID); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, dh.integ)
	got, _ := dh.store.run(run.ID)
	if !got.CancelRequested || got.Status == StatusCompleted || got.Conclusion != "" {
		t.Fatalf("unconfirmed cancellation reported complete: %+v", got)
	}
	if final := dh.lastUpdate(t).Run; final.Status == "completed" {
		t.Fatalf("GitHub was told cancellation completed: %+v", final)
	}
	sent := len(dh.exec.sentKeys())
	dh.exec.mu.Lock()
	dh.exec.cancelError = nil
	dh.exec.mu.Unlock()
	peer := dh.peer("agent-b")
	peer.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(leaseStaleAfter + time.Second) } })
	if err := peer.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, peer)
	got, _ = dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled || got.DriverNodeID != "agent-b" {
		t.Fatalf("replacement did not finish cancellation: %+v", got)
	}
	if len(dh.exec.sentKeys()) != sent || len(dh.exec.cancelled()) != 2 {
		t.Fatalf("recovery reran work instead of retrying cancellation: steps=%v cancels=%v", dh.exec.sentKeys(), dh.exec.cancelled())
	}
}

func TestCleanupFailureLeavesReceiptForAnotherDriverWithoutRepeatingWork(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	dh.exec.ackError = errors.New("pod deletion remains unconfirmed")
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	got, _ := dh.store.run(run.ID)
	if got.Status == StatusCompleted || got.Conclusion != "" {
		t.Fatalf("cleanup failure concluded the run: %+v", got)
	}
	if sent := dh.exec.sentKeys(); len(sent) != 1 || sent[0] != "checks.vet" {
		t.Fatalf("dependent work ran before cleanup: %v", sent)
	}
	if len(dh.work.receiptsOf("checks.vet")) != 1 {
		t.Fatal("cleanup failure lost the command receipt")
	}
	if dh.lastUpdate(t).Run.Conclusion == "success" {
		t.Fatal("GitHub received success before cleanup")
	}
	dh.exec.mu.Lock()
	dh.exec.ackError = nil
	dh.exec.mu.Unlock()
	peer := dh.peer("agent-b")
	peer.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(leaseStaleAfter + time.Second) } })
	if err := peer.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, peer)
	got, _ = dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess {
		t.Fatalf("another driver did not finish cleanup and the run: %+v", got)
	}
	count := 0
	for _, key := range dh.exec.sentKeys() {
		if key == "checks.vet" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the recorded command executed %d times", count)
	}
	acks := dh.exec.acknowledged()
	if len(acks) < 2 || acks[0].StepKey != "checks.vet" || acks[1].StepKey != "checks.vet" {
		t.Fatalf("replacement did not retry cleanup: %+v", acks)
	}
}

func TestRecoveryCleansOtherFinishedJobsWhenOneReceiptRemainsUnconfirmed(t *testing.T) {
	dh := newDriveHarness(t, driveManifest)
	// All commands and their receipts finish, but the original replica loses
	// its final write. Recovery must reconcile the complete recorded batch.
	dh.integ.Configure(func(d *Deps) {
		d.Store = &flakyStore{Store: dh.store, fail: func(p RunPatch) error {
			if p.Status != nil && *p.Status == StatusCompleted {
				return errors.New("final write unavailable")
			}
			return nil
		}}
	})
	run := dh.openRun(t, prOpening())
	deliver(t, dh.integ, run)
	if len(dh.exec.sentKeys()) != 3 {
		t.Fatalf("fixture did not execute all three commands: %v", dh.exec.sentKeys())
	}
	dh.exec.mu.Lock()
	dh.exec.ackErrors = map[string]error{"checks.vet": errors.New("creation remains unresolved")}
	dh.exec.mu.Unlock()
	before := len(dh.exec.acknowledged())
	peer := dh.peer("agent-b")
	peer.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(leaseStaleAfter + time.Second) } })
	if err := peer.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, peer)
	var keys []string
	for _, req := range dh.exec.acknowledged()[before:] {
		keys = append(keys, req.StepKey)
		if len(req.Secrets) != 0 {
			t.Fatal("cleanup carried execution secrets")
		}
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"checks.vet", "tests.lint", "tests.unit"}) {
		t.Fatalf("one unconfirmed cleanup starved other finished Jobs: %v", keys)
	}
	got, _ := dh.store.run(run.ID)
	if got.Status == StatusCompleted || got.Conclusion != "" || len(dh.exec.sentKeys()) != 3 {
		t.Fatalf("unconfirmed cleanup completed or repeated work: %+v; sent %v", got, dh.exec.sentKeys())
	}
	dh.exec.mu.Lock()
	dh.exec.ackErrors = nil
	dh.exec.mu.Unlock()
	last := dh.peer("agent-c")
	last.Configure(func(d *Deps) { d.Now = func() time.Time { return testNow.Add(2*leaseStaleAfter + 2*time.Second) } })
	if err := last.RecoverRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDrives(t, last)
	got, _ = dh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionSuccess || len(dh.exec.sentKeys()) != 3 {
		t.Fatalf("confirmed cleanup did not finish without replay: %+v; sent %v", got, dh.exec.sentKeys())
	}
}
