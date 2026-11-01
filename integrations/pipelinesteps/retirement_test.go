package pipelinesteps

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
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

// createdSecret is the step's Secret as the API server holds one: the sweep
// judges nothing without a creation time, so a fixture with none would keep the
// Secret whatever the sweep's own rules said.
func createdSecret(h *rtHarness, run StepRun) Secret {
	secret := BuildSecret(h.cfg, run, testJobName, rtCloneToken)
	secret.Metadata.CreationTimestamp = h.clock.Now()
	return secret
}

// Past its run's deadline and retention the sweep judges a claim by its stop
// marker too: production kept 13 such Secrets, the oldest 13 days past its
// deadline, each holding a clone token, because any marker at all meant "keep".
func TestMaintenanceWaitsOutALatePostThenCollectsTheAttempt(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(createdSecret(h, run))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	// Stopped only now, long past the run's deadline: the marker is young, and
	// a POST that read no marker a moment ago may still land.
	h.clock.Advance(48 * time.Hour)
	_ = h.r.Ack(context.Background(), AckRequest{JobName: testJobName})
	if _, _, err := h.r.reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.c.hasSecret(testSecretName) || !h.c.hasSecret(testJobName+retirementSuffix) {
		t.Fatal("maintenance erased creation evidence while its POST could still land")
	}
	h.clock.Advance(latePostWindow)
	if _, _, err := h.r.reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.c.hasSecret(testSecretName) {
		t.Fatal("maintenance kept a clone token past the last moment its POST could land")
	}
	if !h.c.hasSecret(testJobName + retirementSuffix) {
		t.Fatal("the stop marker went before its own retention: a late envelope could start the attempt")
	}
	h.clock.Advance(48 * time.Hour)
	if _, _, err := h.r.reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.c.hasSecret(testJobName + retirementSuffix) {
		t.Fatal("resolved expired stop marker was not collected")
	}
}

// A marker that cannot be read proves nothing about how long the attempt has
// been stopped, so the sweep leaves the Secret for its next pass.
func TestMaintenanceKeepsAClaimWhoseStopMarkerCannotBeRead(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(createdSecret(h, run))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	h.clock.Advance(48 * time.Hour)
	_ = h.r.Ack(context.Background(), AckRequest{JobName: testJobName})
	h.clock.Advance(latePostWindow)
	retirementReadClient(t, h, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == kubeSecrets+"/"+testJobName+retirementSuffix {
			return retirementReadStatus(r, http.StatusServiceUnavailable), nil
		}
		return nil, nil
	})
	if _, _, err := h.r.reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !h.c.hasSecret(testSecretName) {
		t.Fatal("maintenance erased creation evidence on a read that answered nothing")
	}
}

func TestMaintenanceReapsTheMarkerOfAReleasedClaim(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	_ = h.r.Ack(context.Background(), AckRequest{JobName: testJobName})
	// The creator proves it never sent a POST, so the claim can be released.
	if err := s.releaseUnusedCreation(); err != nil {
		t.Fatal(err)
	}
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(48 * time.Hour)
	if _, _, err := h.r.reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.c.hasSecret(testJobName + retirementSuffix) {
		t.Fatal("resolved expired stop marker was not collected")
	}
}

// Run 131819ff on 2026-10-29: the cancel landed in the same second as the
// step's Job POST. The creator's context ended with the POST unanswered, so the
// claim stayed `creating` over no Job, with nobody left who could ever change
// it, and every cancel after that was refused: the run read in_progress, with
// every step terminal, until a person looked. Another replica answers the cancel
// here, because the claim and its marker are all a cancel has to go on.
func TestACancelThatInterruptsAJobPostConfirmsOnceThePostCanNoLongerLand(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.c.with(func(c *rtCluster) {
		c.createJobAnswers = []kubeAnswer{rtUnavailable}
		c.onJobCreate = func(*rtCluster, int) { cancel() }
	})
	rtWantCode(t, h.await(t, h.start(ctx, run)), pl.OutcomeCancelled, pl.CodeStepCancelled)
	if h.c.hasJob(testJobName) || !strings.HasPrefix(h.c.secretNow(t, testSecretName).Metadata.Annotations[annotCreation], "creating ") {
		t.Fatal("the fixture is not production's state: a creating claim over no Job")
	}

	config := h.cfg
	config.NodeID = rtOther
	other := h.newRunner(config)
	if _, err := other.CancelRun(context.Background(), CancelRequest{RunID: run.RunID}); err == nil {
		t.Fatal("a cancel was confirmed while the POST it interrupted could still land")
	}
	if !h.c.hasSecret(testSecretName) {
		t.Fatal("cleanup destroyed creation evidence while the POST could still land")
	}

	h.clock.Advance(latePostWindow)
	if _, err := other.CancelRun(context.Background(), CancelRequest{RunID: run.RunID}); err != nil {
		t.Fatalf("the run can never conclude: %v", err)
	}
	if absent, err := other.receiptResourcesAbsent(context.Background(), testJobName); err != nil || !absent {
		t.Fatalf("cleanup: %v %v", absent, err)
	}
	if !h.c.hasSecret(testJobName + retirementSuffix) {
		t.Fatal("the stop marker went with the claim: a late envelope could start the attempt")
	}
	if res := h.run(t, run); res.Status != pl.OutcomeCancelled {
		t.Fatalf("a late envelope: %+v", res)
	}
	if n := len(h.c.requestsFor(http.MethodPost, kubeJobs)); n != 1 {
		t.Fatalf("resolving the claim repeated its POST: %d", n)
	}
}

// The same wedge reached with no cancel at all: a POST whose answer was lost
// fails the step uncertain, and its receipt's cleanup could never be confirmed,
// so the drive stopped for recovery at every pass and the run never ended.
func TestAnUncertainStepsReceiptCleansUpOnceItsPostCanNoLongerLand(t *testing.T) {
	h := newRunnerHarness(t)
	h.c.with(func(c *rtCluster) { c.createJobAnswers = []kubeAnswer{rtUnavailable} })
	wantFailure(t, h.run(t, rtRun()), pl.OutcomeFailed, pl.CodeExecutionUncertain)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("a receipt was cleaned up while its POST could still land")
	}
	h.clock.Advance(latePostWindow)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatalf("the run can never conclude: %v", err)
	}
	if absent, err := h.r.receiptResourcesAbsent(context.Background(), testJobName); err != nil || !absent {
		t.Fatalf("cleanup: %v %v", absent, err)
	}
}

// The wait runs from the stop marker, because that is what a creator cannot get
// past. A claim's own age bounds nothing: its creator may have spent those hours
// on a hung API server, and it sends the moment a read finds no marker.
func TestAnOldClaimStillWaitsOutItsStopMarker(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	h.clock.Advance(2 * time.Hour)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("a stop written this instant was treated as having outlasted a POST")
	}
	// The slowest POST the fence lets through lands 180 s after the marker's
	// stamp: 60 s for the marker's own write to commit, 30 s for the read's answer,
	// and 30 s on the client plus 60 s on the API server for the POST.
	h.clock.Advance(180 * time.Second)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("cleanup stopped waiting while a POST the fence let through could still land")
	}
	h.clock.Advance(latePostWindow - 180*time.Second - time.Second)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("cleanup stopped waiting before its window ended")
	}
	if !h.c.hasSecret(testSecretName) {
		t.Fatal("cleanup destroyed creation evidence before its window ended")
	}
	h.clock.Advance(time.Second)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatal(err)
	}
}

// A marker with no creation time cannot say how long the attempt has been
// stopped. An API server always stamps one, so this holds only the direction a
// reading of a zero time fails in: it would read as stopped for ever.
func TestAStopMarkerWithNoCreationTimeDoesNotEndTheWait(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	s := &step{r: h.r, run: run, jobName: testJobName, ctx: context.Background()}
	if held, err := s.claimCreation(); err != nil || !held {
		t.Fatalf("claim: %v %v", held, err)
	}
	h.c.putSecret(Secret{APIVersion: "v1", Kind: "Secret", Type: "Opaque", Metadata: ObjectMeta{
		Name: testJobName + retirementSuffix, Labels: map[string]string{LabelManagedBy: ManagedBy},
	}})
	h.clock.Advance(1000 * time.Hour)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("a marker that vouches for nothing ended the wait")
	}
	if !h.c.hasSecret(testSecretName) {
		t.Fatal("cleanup destroyed creation evidence on a marker that vouches for nothing")
	}
}

// A POST can land while cleanup is already past its read of the Job. The absence
// check reads again, so cleanup does not confirm over that Job, and the next
// cleanup deletes it as it would any Job.
func TestAPostTheLastReadMissedIsCaughtByTheAbsenceCheck(t *testing.T) {
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
	h.clock.Advance(latePostWindow)
	late := h.existingJob(t, run, nil)
	retirementReadClient(t, h, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete && r.URL.Path == kubeSecrets+"/"+testSecretName {
			h.c.putJob(late, rtRunningScript(testJobName))
		}
		return nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := h.r.Ack(ctx, AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("cleanup confirmed over a Job that landed while it ran")
	}
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatal(err)
	}
	if absent, err := h.r.receiptResourcesAbsent(context.Background(), testJobName); err != nil || !absent {
		t.Fatalf("cleanup: %v %v", absent, err)
	}
}

// A Secret that predates the claim protocol says nothing about its POST, so it
// waits out the window as a claim does.
func TestASecretWithNoCreationStateWaitsOutTheWindowToo(t *testing.T) {
	h := newRunnerHarness(t)
	secret := BuildSecret(h.cfg, rtRun(), testJobName, rtCloneToken)
	delete(secret.Metadata.Annotations, annotCreation)
	h.c.putSecret(secret)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
		t.Fatal("a Secret with no creation state was treated as proof that no POST was sent")
	}
	if !h.c.hasSecret(testSecretName) {
		t.Fatal("cleanup destroyed a Secret whose POST could still land")
	}
	h.clock.Advance(latePostWindow)
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatalf("a run could never conclude: %v", err)
	}
}

// Each marker read can retry for over five minutes against a hung API server,
// and cleanup times its wait from the attempt's marker, so that marker is read
// last. Read first, it could pass, and the stop and the whole wait could then come
// and go during the run marker's retries, with the POST leaving after cleanup had
// confirmed. Another replica stops the attempt here during the post-claim fence's
// read of the run's marker, and the fake clock stands in for those retries.
func TestAStopThatLandsDuringTheFenceStopsThePost(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	config := h.cfg
	config.NodeID = rtOther
	other := h.newRunner(config)
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, ""))
	var reads atomic.Int32
	retirementReadClient(t, h, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == kubeSecrets+"/"+runRetirementName(run.RunID) && reads.Add(1) == 3 {
			if err := other.Ack(context.Background(), AckRequest{JobName: testJobName}); err == nil {
				t.Error("a stop written this instant was treated as having outlasted a POST")
			}
			h.clock.Advance(latePostWindow)
			if err := other.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
				t.Errorf("cleanup past the window: %v", err)
			}
		}
		return nil, nil
	})
	if res := h.run(t, run); res.Status != pl.OutcomeCancelled {
		t.Fatalf("the stopped attempt: %+v (%+v)", res, res.Failure)
	}
	if n := len(h.c.requestsFor(http.MethodPost, kubeJobs)); n != 0 {
		t.Fatalf("a POST left after cleanup had confirmed: %d", n)
	}
}
