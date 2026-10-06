package pipelinerun

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
