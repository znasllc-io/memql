package pipelinesteps

import (
	"context"
	"net/http"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestReceiptRetiresAQueuedAttemptAgainstLateInitialDelivery(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatal(err)
	}
	// This is the original envelope, not a RecoverOnly request. A delayed
	// stream or queue must not recreate the attempt after its receipt landed.
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, ""))
	res := h.run(t, run)
	if res.Status == pl.OutcomeSucceeded || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
		t.Fatal("a late initial delivery executed an acknowledged attempt")
	}
}

func TestRunRetirementRejectsAnEnvelopeThatArrivesAfterCancellation(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	if _, err := h.r.CancelRun(context.Background(), CancelRequest{RunID: run.RunID}); err != nil {
		t.Fatal(err)
	}
	res := h.run(t, run)
	if res.Status != pl.OutcomeCancelled || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 || h.c.hasSecret(testSecretName) {
		t.Fatal("late request escaped the cancelled run's durable stop marker")
	}
}

func TestRetirementWaitsForLatePostThenCleansItWithoutReexecution(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("unresolved create was acknowledged")
	}
	// Admission of the already-sent request happens after the first ack.
	h.c.putJob(h.existingJob(t, run, nil), rtRunningScript(testJobName))
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatal(err)
	}
	if absent, err := h.r.receiptResourcesAbsent(context.Background(), testJobName); err != nil || !absent {
		t.Fatalf("cleanup: %v %v", absent, err)
	}
	h.run(t, run)
	if len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
		t.Fatal("late initial delivery repeated the retired attempt")
	}
}

func TestMaintenanceRetainsUnresolvedCreationAndReapsResolvedMarkers(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	_ = h.r.Ack(context.Background(), AckRequest{JobName: testJobName})
	h.clock.Advance(48 * time.Hour)
	if _, _, err := h.r.reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.c.hasSecret(testSecretName) || !h.c.hasSecret(testJobName+retirementSuffix) {
		t.Fatal("maintenance erased unresolved creation evidence")
	}
	// The creator proves it never sent a POST, so the claim can be released.
	if err := s.releaseUnusedCreation(); err != nil {
		t.Fatal(err)
	}
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.r.reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.c.hasSecret(testJobName + retirementSuffix) {
		t.Fatal("resolved expired stop marker was not collected")
	}
}

func TestReceiptCannotConfirmCleanupAcrossAnUnresolvedCreate(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("absent Job was treated as proof that an in-flight POST cannot still arrive")
	}
	if !h.c.hasSecret(testSecretName) {
		t.Fatal("cleanup destroyed the unresolved creation evidence")
	}
}
