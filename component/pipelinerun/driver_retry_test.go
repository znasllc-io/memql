package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/pipelines"
)

// driver_retry_test.go -- the driver's attempt loop (spec 2026-10-28 fleet-first
// retries, section 3): every attempt journaled before it starts and its outcome
// before the next, a step that never reads finished between attempts, the
// previous attempt's resources acknowledged before the next is sent, and waits
// that end on a cancel or at the run's ceiling.
//
// No test here sleeps a retry delay. The harness installs the driver's timer
// hook (Deps.retryTimer): every wait the driver starts between two attempts is
// recorded and, unless a test says otherwise, fires at once.

// One step each, so that a step's journal reads as one story.
const retryFleetManifest = `formatVersion: 1
name: shop
pipeline:
  stages:
    - name: build
      steps:
        - name: mac
          placement: fleet
          execution: native
          platform: darwin/arm64
          run: make mac
`

const retryClusterManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
          secrets: [SHOP_TOKEN]
`

const retryPoolManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          placement: pool
          platform: linux/amd64
          run: go vet ./...
`

// retryHarness is a drive harness with a clock the test moves and a driver
// whose waits between attempts never sleep.
type retryHarness struct {
	*driveHarness
	clock *driveClock

	mu sync.Mutex
	// waits is every wait the driver started between two attempts, in order.
	waits []time.Duration
	// timeline is what happened, in order: each Execute ("execute N") and each
	// acknowledgement ("acknowledge N"), by attempt.
	timeline []string
	// timer, when set, answers the driver's wait in place of firing at once.
	timer func(time.Duration) <-chan time.Time
}

func newRetryHarness(t *testing.T, manifest string, fleet bool) *retryHarness {
	t.Helper()
	dh := newDriveHarness(t, manifest)
	if fleet {
		dh.p.Compute = pipelines.ComputeClusterAndFleet
		dh.store.addPipeline(dh.p)
	}
	rh := &retryHarness{driveHarness: dh, clock: &driveClock{at: testNow}}
	dh.integ.Configure(func(d *Deps) {
		d.Now = rh.clock.now
		d.retryTimer = rh.retryTimer
	})
	dh.exec.onAcknowledge = func(req pipelines.StepRequest) { rh.note("acknowledge %d", req.Attempt) }
	return rh
}

func (rh *retryHarness) retryTimer(d time.Duration) <-chan time.Time {
	rh.mu.Lock()
	rh.waits = append(rh.waits, d)
	custom := rh.timer
	rh.mu.Unlock()
	if custom != nil {
		return custom(d)
	}
	fired := make(chan time.Time, 1)
	fired <- rh.clock.now()
	return fired
}

func (rh *retryHarness) note(format string, args ...any) {
	rh.mu.Lock()
	rh.timeline = append(rh.timeline, fmt.Sprintf(format, args...))
	rh.mu.Unlock()
}

func (rh *retryHarness) waited() []time.Duration {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	return slices.Clone(rh.waits)
}

func (rh *retryHarness) events() []string {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	return slices.Clone(rh.timeline)
}

// installComputePolicy makes the installation's compute placement policy return
// policy. It is captured when a run opens, so call it before openRun.
func installComputePolicy(t *testing.T, dh *driveHarness, policy string) {
	t.Helper()
	a := workflowSource(t, "@template\nautomation pipelineComputePlacementPolicy { args { repository string! pipelineId string! ownerUserId string! event string! } return "+policy+" }")
	dh.integ.Configure(func(d *Deps) { bindTestWorkflow(d, a) })
}

// nothingRanResult is an attempt whose command provably never started.
func nothingRanResult(code, message string) pipelines.StepResult {
	return pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: -1,
		Failure: &pipelines.Failure{Code: code, Message: message}, AttemptClass: pipelines.AttemptNothingRan}
}

// onMachine is res as the fleet answers it: run on one machine of the owner's.
func onMachine(res pipelines.StepResult, machine string) pipelines.StepResult {
	res.Where = pipelines.Where{Surface: pipelines.PlacementFleet, WorkerID: machine, NodeID: "agent-a"}
	return res
}

// onTheCluster is res as a Job answers it.
func onTheCluster(res pipelines.StepResult, req pipelines.StepRequest) pipelines.StepResult {
	res.Where = pipelines.Where{Surface: pipelines.PlacementCluster, NodeID: "workbench-0", JobName: fmt.Sprintf("job-%s-%d", req.StepKey, req.Attempt)}
	return res
}

// machineFor is the machine the fleet routes a placed attempt to, out of
// machines in the owner's routing order: the pinned one, or the first not
// excluded.
func machineFor(p *pipelines.AttemptPlacement, machines []string) string {
	if p != nil && p.PinWorker != "" {
		return p.PinWorker
	}
	for _, m := range machines {
		if p == nil || !slices.Contains(p.ExcludeWorkers, m) {
			return m
		}
	}
	return ""
}

// intentsOf is every running version of key's row the driver wrote: one intent
// per attempt (the pending version the work run was opened with is not one).
func (dh *driveHarness) intentsOf(key string) []journalCall {
	var out []journalCall
	for _, c := range dh.work.callsNamed("createWorkStep") {
		if argString(c.Args, "key") == key && argString(c.Args, "status") == WorkStepRunning {
			out = append(out, c)
		}
	}
	return out
}

func attemptsOf(c journalCall) ([]attemptRecord, time.Time) {
	return attemptsFromBinding(argObject(c.Args, "binding"))
}

// A fleet step whose first attempt never started its command (the machine's
// fetch failed) passes on its second, pinned to the same machine. Each attempt's
// intent is journaled before it is sent and attempt 1's outcome before attempt 2
// starts, the row reads running between them, and attempt 1's resources are
// acknowledged before attempt 2 is sent (ruling 13).
func TestARetriedStepPassesOnItsSecondAttempt(t *testing.T) {
	rh := newRetryHarness(t, retryFleetManifest, true)
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		rh.note("execute %d", req.Attempt)
		if req.Attempt == 1 {
			return onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The machine's git fetch failed before the command ran."), "msi-raider"), nil
		}
		return onMachine(passed(req), "msi-raider"), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	got, _ := rh.store.run(run.ID)
	if got.Conclusion != ConclusionSuccess {
		t.Fatalf("the run ended %s (%s %q); want success on the step's second attempt", got.Conclusion, got.FailureCode, got.FailureMessage)
	}
	sent := rh.exec.sent()
	if len(sent) != 2 {
		t.Fatalf("the executor was handed %d attempts, want 2", len(sent))
	}
	if sent[0].Attempt != 1 || sent[0].Placement != nil {
		t.Errorf("attempt 1 goes where its manifest placed it: attempt %d, placement %s", sent[0].Attempt, describe(sent[0].Placement))
	}
	if sent[1].Attempt != 2 || sent[1].RecoverOnly || !reflect.DeepEqual(sent[1].Placement, pinnedTo("msi-raider", 120)) {
		t.Errorf("attempt 2 = attempt %d, recover-only %v, placement %s; want a fresh attempt 2 pinned to msi-raider, waiting 120 s for it",
			sent[1].Attempt, sent[1].RecoverOnly, describe(sent[1].Placement))
	}
	if waits := rh.waited(); !slices.Equal(waits, []time.Duration{30 * time.Second}) {
		t.Errorf("the driver waited %v between the attempts, want the policy's first delay, 30s", waits)
	}
	if order := rh.events(); !slices.Equal(order, []string{"execute 1", "acknowledge 1", "execute 2", "acknowledge 2"}) {
		t.Errorf("the order was %v; attempt 1's resources must be acknowledged before attempt 2 is sent", order)
	}

	const key = "build.mac"
	intents := rh.intentsOf(key)
	if len(intents) != 2 {
		t.Fatalf("%d intents, want one per attempt", len(intents))
	}
	if fmt.Sprint(intents[0].Args["attempt"]) != "1" || intents[0].Args["binding"] != nil {
		t.Errorf("attempt 1's intent is what Step always wrote: %v", intents[0].Args)
	}
	if fmt.Sprint(intents[1].Args["attempt"]) != "2" || !strings.HasSuffix(argString(intents[1].Args, "idempotencyKey"), ":"+key+":2") {
		t.Errorf("attempt 2's intent: %v", intents[1].Args)
	}
	if records, next := attemptsOf(intents[1]); len(records) != 1 || records[0].Code != pipelines.CodeCloneFailed || !next.IsZero() {
		t.Errorf("attempt 2's intent restates attempt 1 and names no next attempt: %+v, next %s", records, next)
	}

	updates := rh.work.receiptsOf(key)
	if len(updates) != 2 {
		t.Fatalf("%d updates of the step, want attempt 1's outcome and the receipt", len(updates))
	}
	between := updates[0].Args
	if argString(between, "status") != WorkStepRunning {
		t.Errorf("between the attempts the row reads %q; a step with an attempt to come is running", argString(between, "status"))
	}
	for _, field := range []string{"finishedAt", "errorCode", "errorMessage", "result", "durationMs"} {
		if between[field] != nil {
			t.Errorf("the outcome between attempts wrote %s, which only a receipt writes: %v", field, between)
		}
	}
	records, next := attemptsOf(updates[0])
	if len(records) != 1 || records[0].Attempt != 1 || records[0].Class != pipelines.AttemptNothingRan ||
		records[0].Code != pipelines.CodeCloneFailed || records[0].Surface != pipelines.PlacementFleet || records[0].WorkerID != "msi-raider" {
		t.Errorf("attempt 1's record = %+v", records)
	}
	if !next.Equal(testNow.Add(30 * time.Second)) {
		t.Errorf("the next attempt may start at %s, want %s", next, testNow.Add(30*time.Second))
	}

	receipt := updates[1].Args
	binding := argObject(receipt, "binding")
	if argString(receipt, "status") != WorkStepDone || binding["surface"] != "fleet" || binding["workerId"] != "msi-raider" {
		t.Errorf("the receipt is attempt 2's: %v", receipt)
	}
	if _, stale := binding[bindingNextAttemptAt]; stale {
		t.Errorf("the receipt kept the time of a next attempt: %v", binding)
	}
	records, _ = attemptsFromBinding(binding)
	if len(records) != 2 || records[0].Code != pipelines.CodeCloneFailed || records[1].Attempt != 2 || records[1].Class != "" || records[1].Code != "" {
		t.Errorf("the receipt restates the whole history: %+v", records)
	}
	if row := rh.work.stepRow(key); row["status"] != WorkStepDone || fmt.Sprint(row["attempt"]) != "2" {
		t.Errorf("the row reads %v at attempt %v, want done at 2", row["status"], row["attempt"])
	}
}

// A step whose command keeps asking to be run again (exit 75) stops at the
// engine's cap, whatever the policy allows: four attempts on the first machine
// (sameMachine: 3), four on the next, and no ninth.
func TestAFailingStepStopsAtTheEngineCap(t *testing.T) {
	rh := newRetryHarness(t, retryFleetManifest, true)
	installComputePolicy(t, rh.driveHarness, "{fleetWaitSeconds: 0, cloudFallback: false, retries: {sameMachine: 3, delaysSeconds: [30, 120], otherMachines: true}}")
	machines := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8", "m9"}
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		return onMachine(pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: pipelines.ExitRetryRequested,
			AttemptClass: pipelines.AttemptRetryRequested}, machineFor(req.Placement, machines)), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	sent := rh.exec.sent()
	if len(sent) != pipelines.MaxStepAttempts {
		t.Fatalf("the executor was handed %d attempts, want exactly %d", len(sent), pipelines.MaxStepAttempts)
	}
	var ran []string
	for i, req := range sent {
		if req.Attempt != i+1 {
			t.Errorf("request %d carried attempt %d", i+1, req.Attempt)
		}
		ran = append(ran, machineFor(req.Placement, machines))
	}
	if want := []string{"m1", "m1", "m1", "m1", "m2", "m2", "m2", "m2"}; !slices.Equal(ran, want) {
		t.Errorf("the attempts ran on %v, want %v", ran, want)
	}
	// The policy's delays, the last repeating, before each same-machine retry;
	// none before the move to m2.
	if want := []time.Duration{30 * time.Second, 120 * time.Second, 120 * time.Second, 30 * time.Second, 120 * time.Second, 120 * time.Second}; !slices.Equal(rh.waited(), want) {
		t.Errorf("waits %v, want %v", rh.waited(), want)
	}
	updates := rh.work.receiptsOf("build.mac")
	if len(updates) != pipelines.MaxStepAttempts {
		t.Fatalf("%d updates, want seven outcomes and the receipt", len(updates))
	}
	receipt := updates[len(updates)-1]
	records, next := attemptsOf(receipt)
	if argString(receipt.Args, "status") != WorkStepFailed || len(records) != pipelines.MaxStepAttempts || !next.IsZero() {
		t.Errorf("the receipt: %s with %d records, next %s; want failed with all 8", argString(receipt.Args, "status"), len(records), next)
	}
	if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionFailure {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// Review Focus 2: a cancel during the wait between two attempts settles the
// step cancelled at once -- no further attempt is sent, and the receipt restates
// the history without the time of an attempt that will never start.
func TestCancelDuringARetryWaitSettlesCancelled(t *testing.T) {
	rh := newRetryHarness(t, retryFleetManifest, true)
	waiting := make(chan time.Duration, 1)
	rh.timer = func(d time.Duration) <-chan time.Time {
		waiting <- d
		return make(chan time.Time) // never fires: only the cancel ends this wait
	}
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt > 1 {
			t.Errorf("attempt %d was sent after the run was cancelled", req.Attempt)
			return passed(req), nil
		}
		return onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The fetch failed."), "msi-raider"), nil
	}
	run := rh.openRun(t, prOpening())
	rh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the driver never began waiting for the step's next attempt")
	}
	if err := rh.integ.RequestCancel(context.Background(), run.ID, "v1:identity:user:"+ownerID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitDrives(t, rh.integ)

	if n := len(rh.exec.sent()); n != 1 {
		t.Errorf("the executor was handed %d attempts, want only the first", n)
	}
	updates := rh.work.receiptsOf("build.mac")
	if len(updates) != 2 || argString(updates[0].Args, "status") != WorkStepRunning || argString(updates[1].Args, "status") != WorkStepCancelled {
		t.Fatalf("the step's updates = %+v; want attempt 1's outcome, then a cancelled receipt", updates)
	}
	if records, next := attemptsOf(updates[1]); len(records) != 1 || records[0].Code != pipelines.CodeCloneFailed || !next.IsZero() {
		t.Errorf("the cancelled receipt restates attempt 1 and names no next attempt: %+v, next %s", records, next)
	}
	got, _ := rh.store.run(run.ID)
	if got.Status != StatusCompleted || got.Conclusion != ConclusionCancelled {
		t.Errorf("run = %s/%s", got.Status, got.Conclusion)
	}
	if cancels := rh.exec.cancelled(); !slices.Equal(cancels, []string{run.ID}) {
		t.Errorf("the runner is told to stop the run once: %v", cancels)
	}
}

// Review Focus 3: every attempt counts against the run's ceiling. A retry whose
// wait would end with less than a second of the run left is never started, and
// neither is one whose wait outlasted the run: the step keeps attempt 1's
// failure.
func TestRunCeilingStopsTheRetries(t *testing.T) {
	t.Setenv(pipelines.EnvRunCeiling, "5")
	const ceiling = 5 * time.Minute
	imagePull := func(req pipelines.StepRequest) pipelines.StepResult {
		return onTheCluster(nothingRanResult(pipelines.CodeImagePullFailed, "The step's image could not be pulled."), req)
	}
	keepsAttemptOne := func(t *testing.T, receipt journalCall) {
		t.Helper()
		if argString(receipt.Args, "status") != WorkStepFailed || argString(receipt.Args, "errorCode") != pipelines.CodeImagePullFailed {
			t.Errorf("the receipt: %s %s; want attempt 1's failure", argString(receipt.Args, "status"), argString(receipt.Args, "errorCode"))
		}
		if records, next := attemptsOf(receipt); len(records) != 1 || !next.IsZero() {
			t.Errorf("the receipt's history: %+v, next %s; want attempt 1 alone and no next attempt", records, next)
		}
	}

	t.Run("a wait that would end past the ceiling is never started", func(t *testing.T) {
		rh := newRetryHarness(t, retryClusterManifest, false)
		rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
			if req.Attempt > 1 {
				t.Errorf("attempt %d was sent past the run's ceiling", req.Attempt)
				return passed(req), nil
			}
			// Attempt 1 answers with 20 seconds of the run left; the first retry
			// delay is 30.
			rh.clock.set(testNow.Add(ceiling - 20*time.Second))
			return imagePull(req), nil
		}
		run := rh.openRun(t, prOpening())
		deliver(t, rh.integ, run)

		if n := len(rh.exec.sent()); n != 1 {
			t.Errorf("the executor was handed %d attempts, want 1", n)
		}
		if waits := rh.waited(); len(waits) != 0 {
			t.Errorf("the driver began waiting %v for an attempt that could not start", waits)
		}
		updates := rh.work.receiptsOf("checks.vet")
		if len(updates) != 1 {
			t.Fatalf("%d updates; want the receipt alone, no outcome promising another attempt", len(updates))
		}
		keepsAttemptOne(t, updates[0])
	})

	t.Run("a wait that outlasted the run starts no attempt", func(t *testing.T) {
		rh := newRetryHarness(t, retryClusterManifest, false)
		rh.timer = func(time.Duration) <-chan time.Time {
			// The process slept through the wait: the run is at its ceiling when
			// the timer fires.
			rh.clock.set(testNow.Add(ceiling))
			fired := make(chan time.Time, 1)
			fired <- rh.clock.now()
			return fired
		}
		rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
			if req.Attempt > 1 {
				t.Errorf("attempt %d was sent past the run's ceiling", req.Attempt)
				return passed(req), nil
			}
			return imagePull(req), nil
		}
		run := rh.openRun(t, prOpening())
		deliver(t, rh.integ, run)

		if n := len(rh.exec.sent()); n != 1 {
			t.Errorf("the executor was handed %d attempts, want 1", n)
		}
		updates := rh.work.receiptsOf("checks.vet")
		if len(updates) != 2 || argString(updates[0].Args, "status") != WorkStepRunning {
			t.Fatalf("the step's updates = %+v; want attempt 1's outcome and then the receipt", updates)
		}
		keepsAttemptOne(t, updates[1])
		if acks := rh.exec.acknowledged(); len(acks) != 1 || acks[0].Attempt != 1 {
			t.Errorf("acknowledged %d times; attempt 1 is acknowledged once, before the wait", len(acks))
		}
		if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionFailure {
			t.Errorf("run = %s", got.Conclusion)
		}
	})
}

// Review Focus 1: an attempt's record is masked as its receipt is. Attempt 1's
// runner echoed a resolved secret into its message, its Job name and its log
// file id; the binding shows the mask everywhere, and attempt 2 is still handed
// the value (masking is what is written, never what is sent).
func TestAttemptRecordsAreMasked(t *testing.T) {
	rh := newRetryHarness(t, retryClusterManifest, false)
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Secrets["SHOP_TOKEN"] != shopSecret {
			t.Errorf("attempt %d was not handed its secret", req.Attempt)
		}
		if req.Attempt == 1 {
			res := nothingRanResult(pipelines.CodeImagePullFailed, "pulling with token "+shopSecret+" failed")
			res.Where = pipelines.Where{Surface: pipelines.PlacementCluster, NodeID: "workbench-0", JobName: "job-" + shopSecret}
			res.LogFileID = "file-" + shopSecret
			return res, nil
		}
		return passed(req), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionSuccess {
		t.Fatalf("run = %s; want success on attempt 2", got.Conclusion)
	}
	for _, c := range rh.work.recorded() {
		if strings.Contains(c.Raw, shopSecret) {
			t.Errorf("a journal write carries the value: %s", c.Raw)
		}
	}
	if strings.Contains(rh.logs.String(), shopSecret) {
		t.Errorf("a log line carries the value")
	}
	updates := rh.work.receiptsOf("checks.vet")
	if len(updates) != 2 {
		t.Fatalf("%d updates, want attempt 1's outcome and the receipt", len(updates))
	}
	for _, u := range updates {
		records, _ := attemptsOf(u)
		if len(records) == 0 {
			t.Fatalf("no history in %v", u.Args)
		}
		first := records[0]
		if first.Message != "pulling with token *** failed" || first.JobName != "job-***" || first.LogFileID != "file-***" {
			t.Errorf("attempt 1's record = %+v; want every string masked", first)
		}
	}
}

// A pool step that may spend cloud fails nothing ran on every machine, and the
// driver's next place is the cluster. Under a held cloud gate the workbench makes
// no Job and the attempt waits, inside itself, until the run's ceiling: the step
// ends with that failure, and the driver sends nothing after it.
func TestHeldCloudGateMakesTheCloudFailoverWait(t *testing.T) {
	t.Setenv(pipelines.EnvRunCeiling, "30")
	rh := newRetryHarness(t, retryPoolManifest, true)
	installComputePolicy(t, rh.driveHarness, "{fleetWaitSeconds: 5, cloudFallback: true, retries: {sameMachine: 0, delaysSeconds: [0], otherMachines: true}}")
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		switch req.Attempt {
		case 1:
			return onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The fetch failed."), "m1"), nil
		case 2:
			return onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The fetch failed."), "m2"), nil
		case 3:
			// The wait the driver allowed ran out with no machine free.
			res := pipelines.StepResult{Status: pipelines.OutcomeRefused, ExitCode: -1,
				Failure:      &pipelines.Failure{Code: pipelines.CodeNoMachineForNeed, Message: "The wait of 5s that the driver allowed ran out. No command was started."},
				Where:        pipelines.Where{Surface: pipelines.PlacementFleet},
				AttemptClass: pipelines.AttemptNothingRan, NotRetriedHere: true}
			return res, nil
		case 4:
			// The cost guard holds cloud spending: no Job is made, and the attempt
			// is held until its run's ceiling.
			rh.clock.set(testNow.Add(30 * time.Minute))
			res := pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: -1,
				Failure:      &pipelines.Failure{Code: pipelines.CodeRunCeiling, Message: "The run reached its ceiling while cloud spending was held. No Job was created."},
				AttemptClass: pipelines.AttemptFinal}
			return onTheCluster(res, req), nil
		}
		t.Errorf("attempt %d was sent after the step failed at its run's ceiling", req.Attempt)
		return passed(req), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	sent := rh.exec.sent()
	if len(sent) != 4 {
		t.Fatalf("the executor was handed %d attempts, want 4", len(sent))
	}
	want := []*pipelines.AttemptPlacement{nil, failoverFrom(5, "m1"), failoverFrom(5, "m1", "m2"), toCluster}
	for i, req := range sent {
		if !reflect.DeepEqual(req.Placement, want[i]) {
			t.Errorf("attempt %d was placed %s, want %s", i+1, describe(req.Placement), describe(want[i]))
		}
	}
	updates := rh.work.receiptsOf("checks.vet")
	if len(updates) != 4 {
		t.Fatalf("%d updates, want three outcomes and the receipt", len(updates))
	}
	receipt := updates[3]
	if argString(receipt.Args, "status") != WorkStepFailed || argString(receipt.Args, "errorCode") != pipelines.CodeRunCeiling {
		t.Errorf("the receipt: %s %s; want the cluster attempt's run-ceiling failure", argString(receipt.Args, "status"), argString(receipt.Args, "errorCode"))
	}
	if records, _ := attemptsOf(receipt); len(records) != 4 || records[3].Surface != pipelines.PlacementCluster {
		t.Errorf("the history: %+v", records)
	}
	if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionFailure {
		t.Errorf("run = %s", got.Conclusion)
	}
}

// Ruling 13: the next attempt starts only once the previous attempt's
// resources are confirmed gone. When the acknowledgement cannot confirm it, the
// step is not retried and the run stays unfinished for recovery, its row saying
// what attempt 1 proved and when attempt 2 may start.
func TestAnUnconfirmedCleanupStopsTheRetries(t *testing.T) {
	rh := newRetryHarness(t, retryClusterManifest, false)
	rh.exec.ackErrors = map[string]error{"checks.vet": errors.New("foreground deletion still pending")}
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt > 1 {
			t.Errorf("attempt %d was sent before attempt 1's Job was confirmed gone", req.Attempt)
			return passed(req), nil
		}
		return onTheCluster(nothingRanResult(pipelines.CodeImagePullFailed, "The step's image could not be pulled."), req), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	if n := len(rh.exec.sent()); n != 1 {
		t.Errorf("the executor was handed %d attempts, want 1", n)
	}
	if waits := rh.waited(); len(waits) != 0 {
		t.Errorf("the driver waited %v after a cleanup it could not confirm", waits)
	}
	if got, _ := rh.store.run(run.ID); got.Status == StatusCompleted {
		t.Errorf("the run concluded %s; a run whose cleanup is unconfirmed stays unfinished for recovery", got.Conclusion)
	}
	updates := rh.work.receiptsOf("checks.vet")
	if len(updates) != 1 || argString(updates[0].Args, "status") != WorkStepRunning {
		t.Fatalf("the step's updates = %+v; want attempt 1's outcome alone", updates)
	}
	if records, next := attemptsOf(updates[0]); len(records) != 1 || next.IsZero() {
		t.Errorf("the row keeps attempt 1's record and when attempt 2 may start: %+v, next %s", records, next)
	}
}
