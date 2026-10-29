package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
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

// noRouteResult is the one cluster ending that proves the command never started:
// the agent's own refusal, made before any runner took the step
// (noRunnerReached, integrations/pipelinesteps). Every ending a runner reports
// leaves a Job that may yet start the command, and reads outcome unknown.
func noRouteResult() pipelines.StepResult {
	return nothingRanResult(pipelines.CodeRunnerUnavailable, "This agent node has no route to a workbench replica. Nothing ran.")
}

// fleetRecoveryResult is what the fleet answers a recovery of an attempt on a
// machine (integrations/pipelinesteps: Executor.runFleet, classified by
// Execute): a machine holds no Job to re-attach to, so nothing is sent, and
// whether the command ran is unknown.
func fleetRecoveryResult() pipelines.StepResult {
	return pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: -1,
		Failure: &pipelines.Failure{Code: pipelines.CodeExecutionUncertain,
			Message: "A previous driver may have started this fleet command, and no durable result was recorded. Reconcile its outcome before authorizing new work; no replacement command was sent."},
		AttemptClass: pipelines.AttemptOutcomeUnknown}
}

// driveIntoTheWait has agent-a drive run until its step waits between two
// attempts, and returns then. The wait's timer is fire: agent-a stays in the
// wait until fire delivers (never, when fire is nil) or its lease's stop ends it.
func (rh *retryHarness) driveIntoTheWait(t *testing.T, run Run, fire <-chan time.Time) {
	t.Helper()
	waiting := make(chan struct{})
	var once sync.Once
	rh.mu.Lock()
	rh.timer = func(time.Duration) <-chan time.Time {
		once.Do(func() { close(waiting) })
		return fire
	}
	rh.mu.Unlock()
	rh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("agent-a never began waiting for the step's next attempt")
	}
}

// replica is another agent replica over rh's rows, on its own clock, its waits
// between attempts answered by timer -- at once, when timer is nil.
func (rh *retryHarness) replica(node string, now func() time.Time, timer func(time.Duration) <-chan time.Time) *Integration {
	if timer == nil {
		timer = func(time.Duration) <-chan time.Time {
			fired := make(chan time.Time, 1)
			fired <- now()
			return fired
		}
	}
	peer := rh.peer(node)
	peer.Configure(func(d *Deps) {
		d.Now = now
		d.retryTimer = timer
	})
	return peer
}

// stopRenewing ages runID's lease as a replica's that stopped renewing it: the
// run is another replica's to take over.
func (dh *driveHarness) stopRenewing(runID string) {
	dh.store.mu.Lock()
	r := dh.store.runs[runID]
	r.DriverHeartbeatAt = testNow.Add(-3 * time.Minute)
	dh.store.runs[runID] = r
	dh.store.mu.Unlock()
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

// The wait between two attempts belongs to the workflow call that runs the
// step: when that call ends -- the workflow stopping around it -- the wait ends
// with it, and the step writes nothing more. Not the next attempt's intent,
// which would fail on the ended call and read as a journal outage: the row
// still says when the next attempt is due, for whichever drive carries the run
// on.
func TestARetryWaitEndsWithItsWorkflowCall(t *testing.T) {
	work := newFakeWork()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	waiting := make(chan struct{})
	dr := &runDriver{lease: newLease("r1"), log: quiet, d: Deps{
		Now:    func() time.Time { return testNow },
		Logger: quiet,
		retryTimer: func(time.Duration) <-chan time.Time {
			close(waiting)
			return make(chan time.Time) // never fires: only the call's end can end this wait
		},
	}}
	dr.workRun.Store(workjournal.New(work, quiet, "agent-a").Reopen(ownerID, "goal-1", "run-1",
		[]workjournal.StepDecl{{Key: "build.mac", Kind: workjournal.KindDeterministic, StepType: "exec"}}, testNow))
	track := &stepTrack{step: pipelines.Step{Key: "build.mac", Kind: pipelines.StepCommand}, attempt: 1}
	track.setAttempts([]attemptRecord{{Attempt: 1, Class: pipelines.AttemptNothingRan, Code: pipelines.CodeCloneFailed}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	began := make(chan bool, 1)
	go func() {
		began <- dr.beginNextAttempt(ctx, track, pinnedTo("msi-raider", 120), testNow.Add(30*time.Second), testNow.Add(time.Hour),
			failReceipt(pipelines.CodeCloneFailed, "The fetch failed."))
	}()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the step never began waiting for its next attempt")
	}
	cancel()
	select {
	case ok := <-began:
		if ok {
			t.Error("the next attempt began after the workflow call that runs the step ended")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait between two attempts outlived the workflow call that runs the step")
	}
	if calls := work.recorded(); len(calls) != 0 {
		t.Errorf("the step wrote %d journal calls after its call ended: %+v", len(calls), calls)
	}
	if track.attempt != 1 || dr.journalFailed.Load() || dr.lease.isLost() {
		t.Errorf("attempt %d, journal failed %v, lease lost %v; the step stops where it stood, and nothing reads as a journal outage",
			track.attempt, dr.journalFailed.Load(), dr.lease.isLost())
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

const retryRepeatableClusterManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: vet
          run: go vet ./...
          repeatable: true
`

// Ruling 18: a repeatable cluster step whose node went away under its Job
// (pipeline_node_lost: the command may have run) is tried again on the cluster,
// once the first attempt's Job is confirmed gone -- so two copies of it never
// run at once.
func TestARepeatableClusterStepRetriesAnUnknownOutcomeOnTheCluster(t *testing.T) {
	rh := newRetryHarness(t, retryRepeatableClusterManifest, false)
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		rh.note("execute %d", req.Attempt)
		if !req.Step.Repeatable {
			t.Errorf("the fixture's step is not repeatable: %+v", req.Step)
		}
		if req.Attempt == 1 {
			return onTheCluster(pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: -1,
				Failure:      &pipelines.Failure{Code: pipelines.CodeNodeLost, Message: "The node under the step's Job went away before it reported."},
				AttemptClass: pipelines.AttemptOutcomeUnknown}, req), nil
		}
		return onTheCluster(passed(req), req), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionSuccess {
		t.Fatalf("the run ended %s; want success on the step's second attempt", got.Conclusion)
	}
	sent := rh.exec.sent()
	if len(sent) != 2 {
		t.Fatalf("the executor was handed %d attempts, want 2", len(sent))
	}
	if sent[1].Attempt != 2 || sent[1].RecoverOnly || !reflect.DeepEqual(sent[1].Placement, toCluster) {
		t.Errorf("attempt 2 = attempt %d, recover-only %v, placement %s; want a fresh attempt 2 on the cluster",
			sent[1].Attempt, sent[1].RecoverOnly, describe(sent[1].Placement))
	}
	if order := rh.events(); !slices.Equal(order, []string{"execute 1", "acknowledge 1", "execute 2", "acknowledge 2"}) {
		t.Errorf("the order was %v; attempt 1's Job must be confirmed gone before attempt 2 is sent", order)
	}
	if waits := rh.waited(); !slices.Equal(waits, []time.Duration{30 * time.Second}) {
		t.Errorf("waits %v, want the policy's first delay, 30s", waits)
	}
	updates := rh.work.receiptsOf("checks.vet")
	if len(updates) != 2 {
		t.Fatalf("%d updates of the step, want attempt 1's outcome and the receipt", len(updates))
	}
	if records, _ := attemptsOf(updates[0]); len(records) != 1 || records[0].Class != pipelines.AttemptOutcomeUnknown ||
		records[0].Code != pipelines.CodeNodeLost || records[0].Surface != pipelines.PlacementCluster {
		t.Errorf("attempt 1's record = %+v", records)
	}
	if records, _ := attemptsOf(updates[1]); argString(updates[1].Args, "status") != WorkStepDone || len(records) != 2 {
		t.Errorf("the receipt: %s with %+v; want done with both attempts", argString(updates[1].Args, "status"), records)
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

// retryingMachine is the registration a retried step's first attempt ran on, and
// retryingMachineName what the check run calls it: the name its owner sees in Fleet,
// which the dispatcher records with the id.
const (
	retryingMachine     = "3f9c2ab0-1d2e-4f3a-8b5c-6d7e8f9a0b1c"
	retryingMachineName = "MSI Raider"
)

// named is res as the fleet answers it when the dispatcher also says what the
// machine is called.
func named(res pipelines.StepResult, name string) pipelines.StepResult {
	res.Where.MachineName = name
	return res
}

// While a step waits for its next attempt the check run says so. Nothing
// published a thing during the wait before, so a person reading the check saw a
// plain running step for up to minutes. The driver publishes the moment it has
// recorded the attempt's outcome, before the wait begins, and the step's
// receipt then states how it got there.
func TestTheCheckRunSaysAStepIsRetryingWhileItWaits(t *testing.T) {
	rh := newRetryHarness(t, retryFleetManifest, true)
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt == 1 {
			return named(onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The machine's git fetch failed before the command ran."), retryingMachine), retryingMachineName), nil
		}
		return named(onMachine(passed(req), retryingMachine), retryingMachineName), nil
	}
	run := rh.openRun(t, prOpening())
	fire := make(chan time.Time)
	rh.driveIntoTheWait(t, run, fire)

	// The drive is parked before attempt 2; the check run already says why.
	waiting := rh.lastUpdate(t).Run
	if waiting.Status != StatusInProgress {
		t.Errorf("the check run is %s while a step waits; the run is not over", waiting.Status)
	}
	wantWhile := "### Retried steps\n\n" +
		"- `build.mac`: Retrying: attempt 1 failed (`pipeline_clone_failed` on " + retryingMachineName + "); attempt 2 starts after 30s.\n"
	if !strings.HasSuffix(waiting.Output.Summary, wantWhile) {
		t.Errorf("the check run while the step waits:\n%s\nwant it to end\n%s", waiting.Output.Summary, wantWhile)
	}
	if got := rh.exec.sent(); len(got) != 1 {
		t.Errorf("the executor was handed %d attempts while the step waits, want 1", len(got))
	}

	fire <- rh.clock.now()
	waitDrives(t, rh.integ)
	if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionSuccess {
		t.Fatalf("the run ended %s; want success on the step's second attempt", got.Conclusion)
	}
	final := rh.lastUpdate(t).Run
	wantAfter := "### Retried steps\n\n" +
		"- `build.mac`: Passed on attempt 2 of 2. Attempt 1: `pipeline_clone_failed` on " + retryingMachineName + ".\n"
	if final.Status != StatusCompleted || !strings.HasSuffix(final.Output.Summary, wantAfter) {
		t.Errorf("the concluded check run:\n%s\nwant it to end\n%s", final.Output.Summary, wantAfter)
	}
	if strings.Contains(final.Output.Summary, "Retrying") {
		t.Errorf("the concluded check run still says the step is retrying:\n%s", final.Output.Summary)
	}
}

// A step that fails for good after its retries lists every attempt, and never
// reads as retrying once its receipt is written.
func TestTheCheckRunListsEveryAttemptOfAStepThatFailedForGood(t *testing.T) {
	rh := newRetryHarness(t, retryClusterManifest, false)
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt < 2 {
			return noRouteResult(), nil
		}
		// The second attempt ran and its command failed: an ordinary exit is final.
		return onTheCluster(pipelines.StepResult{Status: pipelines.OutcomeFailed, ExitCode: 1, AttemptClass: pipelines.AttemptFinal}, req), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionFailure {
		t.Fatalf("the run ended %s; want failure", got.Conclusion)
	}
	final := rh.lastUpdate(t).Run
	want := "### Failed steps\n\n" +
		"- `checks.vet`: The step's command exited with status 1.\n" +
		"  - Attempt 1: `pipeline_runner_unavailable` on the cluster.\n" +
		"  - Attempt 2: failed on the cluster.\n"
	if !strings.HasSuffix(final.Output.Summary, want) {
		t.Errorf("the concluded check run:\n%s\nwant it to end\n%s", final.Output.Summary, want)
	}
}

// A failover to another machine waits for nothing, so the check run does not
// promise a wait: the next attempt "follows". The publish happens all the same,
// before attempt 2 is sent.
func TestTheCheckRunSaysAFailoverFollows(t *testing.T) {
	const other = "5d8e1f77-9a0b-4c1d-8e2f-3a4b5c6d7e8f"
	rh := newRetryHarness(t, retryFleetManifest, true)
	installComputePolicy(t, rh.driveHarness, "{fleetWaitSeconds: 0, cloudFallback: false, retries: {sameMachine: 0, delaysSeconds: [30], otherMachines: true}}")
	machines := []string{retryingMachine, other}
	names := map[string]string{retryingMachine: retryingMachineName, other: "CyberPC"}
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		machine := machineFor(req.Placement, machines)
		if req.Attempt == 1 {
			return named(onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The fetch failed."), machine), names[machine]), nil
		}
		return named(onMachine(passed(req), machine), names[machine]), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	if sent := rh.exec.sent(); len(sent) != 2 || machineFor(sent[1].Placement, machines) != other {
		t.Fatalf("attempt 2 goes to the other machine: %+v", sent)
	}
	if waits := rh.waited(); len(waits) != 0 {
		t.Fatalf("a failover waits for nothing, and the driver waited %v", waits)
	}
	say := "- `build.mac`: Retrying: attempt 1 failed (`pipeline_clone_failed` on " + retryingMachineName + "); attempt 2 follows.\n"
	said := false
	for _, w := range rh.github.updatedRuns() {
		said = said || (w.Run.Output != nil && strings.HasSuffix(w.Run.Output.Summary, say))
	}
	if !said {
		t.Errorf("no check run write said the step was retrying and the next attempt follows; the writes ended with:\n%s", rh.lastUpdate(t).Run.Output.Summary)
	}
}

// A final check run that did not land is republished from the work run's rows,
// and says what the original would have: the receipt restates the step's whole
// history, so whether a person reads "Passed on attempt 2 of 2" does not depend
// on which write of the report arrived.
func TestARepublishedFinalReportKeepsTheStepsAttempts(t *testing.T) {
	rh := newRetryHarness(t, retryFleetManifest, true)
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt == 1 {
			return named(onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The fetch failed."), retryingMachine), retryingMachineName), nil
		}
		return named(onMachine(passed(req), retryingMachine), retryingMachineName), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)
	finished, _ := rh.store.run(run.ID)

	reports, err := finalStepReports(context.Background(), rh.integ.snapshot(), finished)
	if err != nil {
		t.Fatalf("finalStepReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Key != "build.mac" {
		t.Fatalf("reports = %+v; want the one step", reports)
	}
	want := []pipelines.AttemptReport{
		{Attempt: 1, Code: pipelines.CodeCloneFailed, Where: retryingMachineName},
		{Attempt: 2, Where: retryingMachineName},
	}
	if !reflect.DeepEqual(reports[0].Attempts, want) {
		t.Errorf("the republished step's attempts\n got %+v\nwant %+v", reports[0].Attempts, want)
	}
}

// The check run only ever says a step is retrying once the row holds the
// attempt's outcome: a Retrying write the journal refused stops the drive, and
// nothing is promised on GitHub that the row does not say.
func TestTheCheckRunNeverPromisesARetryTheRowDoesNotHold(t *testing.T) {
	rh := newRetryHarness(t, retryFleetManifest, true)
	rh.work.refuseCall, rh.work.refuseErr, rh.work.refuseTimes = "updateWorkStep", errors.New("journal unavailable"), 1
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt > 1 {
			t.Errorf("attempt %d was sent after its predecessor's outcome could not be recorded", req.Attempt)
			return onMachine(passed(req), retryingMachine), nil
		}
		return onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The fetch failed."), retryingMachine), nil
	}
	run := rh.openRun(t, prOpening())
	deliver(t, rh.integ, run)

	if got, _ := rh.store.run(run.ID); got.Status == StatusCompleted {
		t.Fatalf("a run whose step outcome was not recorded concluded %s", got.Conclusion)
	}
	for _, w := range rh.github.updatedRuns() {
		if w.Run.Output != nil && strings.Contains(w.Run.Output.Summary, "Retrying") {
			t.Errorf("the check run says a step is retrying that the row does not hold:\n%s", w.Run.Output.Summary)
		}
	}
}

// overlapGitHub holds the first check-run write that says a step is retrying
// until it is released, and notes whether two writes were ever in flight at once.
type overlapGitHub struct {
	*fakeGitHub
	held    chan struct{} // closed when the held write has begun
	release chan struct{}
	once    sync.Once

	mu         sync.Mutex
	inFlight   int
	overlapped bool
}

func (g *overlapGitHub) UpdateCheckRun(ctx context.Context, token, repository string, id int64, run githubapp.CheckRun) error {
	g.mu.Lock()
	g.inFlight++
	g.overlapped = g.overlapped || g.inFlight > 1
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}()
	if run.Output != nil && strings.Contains(run.Output.Summary, "Retrying:") {
		hold := false
		g.once.Do(func() { hold = true })
		if hold {
			close(g.held)
			select {
			case <-g.release:
			case <-ctx.Done():
			}
		}
	}
	return g.fakeGitHub.UpdateCheckRun(ctx, token, repository, id, run)
}

func (g *overlapGitHub) state() (inFlight int, overlapped bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight, g.overlapped
}

const retrySiblingsManifest = `formatVersion: 1
name: shop
pipeline:
  image: ghcr.io/acme/toolchain@sha256:abc
  stages:
    - name: checks
      steps:
        - name: slow
          run: make slow
        - name: fast
          run: make fast
`

// Each progress call snapshots the steps and then writes GitHub, so two at once
// could land the older snapshot last. The workflow's own progress, which follows
// a step that settled, and the retry's, which runStep makes, are one at a time:
// here the slow step's retry is being published when its sibling settles, and the
// sibling's progress waits for it.
func TestARetrysPublishAndTheWorkflowsProgressNeverOverlap(t *testing.T) {
	rh := newRetryHarness(t, retrySiblingsManifest, false)
	gh := &overlapGitHub{fakeGitHub: rh.github, held: make(chan struct{}), release: make(chan struct{})}
	rh.integ.Configure(func(d *Deps) { d.GitHub = gh })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gh.release) }) }
	t.Cleanup(func() {
		release()
		waitDrives(t, rh.integ)
	})
	rh.exec.answer = func(ctx context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		switch {
		case req.StepKey == "checks.slow" && req.Attempt == 1:
			return noRouteResult(), nil
		case req.StepKey == "checks.fast":
			// The sibling settles only while the slow step's retry is being published.
			select {
			case <-gh.held:
			case <-ctx.Done():
				return pipelines.StepResult{}, ctx.Err()
			}
		}
		return passed(req), nil
	}
	run := rh.openRun(t, prOpening())
	rh.integ.HandleRunEvent(graphEvent(events.TopicGraphNodeCreated, run))
	select {
	case <-gh.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow step's retry was never published")
	}
	deadline := time.After(10 * time.Second)
	for len(rh.work.receiptsOf("checks.fast")) == 0 {
		select {
		case <-deadline:
			t.Fatal("the sibling never settled while the retry's publish was held")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Its progress has had every chance to reach GitHub; it must be waiting.
	time.Sleep(200 * time.Millisecond)
	if inFlight, overlapped := gh.state(); inFlight != 1 || overlapped {
		t.Errorf("%d check-run writes in flight (overlapped %v) while the retry's publish is held; the sibling's progress must wait for it", inFlight, overlapped)
	}
	release()
	waitDrives(t, rh.integ)
	if _, overlapped := gh.state(); overlapped {
		t.Error("two check-run writes were in flight at once")
	}
	if got, _ := rh.store.run(run.ID); got.Conclusion != ConclusionSuccess {
		t.Errorf("the run ended %s; want success", got.Conclusion)
	}
}

const retryFleetSecretManifest = `formatVersion: 1
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
          secrets: [SHOP_TOKEN]
`

// A machine's name is text the machine's owner chose, and a step's secrets are
// masked wherever the driver writes or says anything: the record, the journal,
// the log and the check run, as every other string of an attempt is.
func TestAMachineNameIsMaskedLikeTheRestOfAnAttemptsRecord(t *testing.T) {
	rh := newRetryHarness(t, retryFleetSecretManifest, true)
	rh.exec.answer = func(_ context.Context, req pipelines.StepRequest) (pipelines.StepResult, error) {
		if req.Attempt == 1 {
			return named(onMachine(nothingRanResult(pipelines.CodeCloneFailed, "The fetch failed."), retryingMachine), "box-"+shopSecret), nil
		}
		return named(onMachine(passed(req), retryingMachine), "box-"+shopSecret), nil
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
	updates := rh.work.receiptsOf("build.mac")
	if len(updates) != 2 {
		t.Fatalf("%d updates, want attempt 1's outcome and the receipt", len(updates))
	}
	for _, u := range updates {
		if records, _ := attemptsOf(u); len(records) == 0 || records[0].MachineName != "box-***" {
			t.Errorf("attempt 1's record = %+v; want the name masked and kept", records)
		}
	}
	for _, w := range rh.github.updatedRuns() {
		if w.Run.Output != nil && strings.Contains(w.Run.Output.Summary, shopSecret) {
			t.Errorf("the check run carries the value:\n%s", w.Run.Output.Summary)
		}
	}
	if final := rh.lastUpdate(t).Run; !strings.Contains(final.Output.Summary, "Attempt 1: `pipeline_clone_failed` on box-\\*\\*\\*.") {
		t.Errorf("the concluded check run names the masked machine:\n%s", final.Output.Summary)
	}
}
