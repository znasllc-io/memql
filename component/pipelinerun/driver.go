package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/component/workjournal"
	"github.com/znasllc-io/memql/core/logger"
)

// driver.go -- an agent replica drives a run (plan Task 10b; design record
// D7, D9, D11, D13).
//
// A run is OPENED queued wherever its cause arrived (open.go). It is DRIVEN
// here, on an agent node that claimed it under the row lease (lease.go): the
// pipeline loaded, a token minted through the owner's grant, the run moved to
// in_progress, the tree read at the commit, the manifest compiled into a plan
// (tree.go), the plan opened as ONE work goal with ONE v1:work:step per step,
// the stages executed strictly in order with each stage's steps at once, and
// the run concluded -- the row, the work run and the check run together.
//
// EVERY FACT A RUN NEEDS TO CONTINUE IS ON A ROW. The lease is the run row's;
// the plan is the manifest at the commit, read again; what finished is the
// work steps' receipts; the package slice a step was given is on its step's
// call; where a step's attempts stand is on its step's binding. So a replica
// that dies mid-run strands nothing: another claims the run once the lease is
// stale (recover.go), keeps every step that has a receipt, re-sends an attempt
// that was running with the SAME attempt number, which the runner re-attaches
// by (run, step key, attempt), and continues a step left between two attempts
// at the next one (resume).
//
// SECRET VALUES LIVE IN ONE PLACE: the StepRequest handed to the executor for
// one step (secrets.go). They are never written to a row and never logged;
// everything the runner reports back that a person reads -- a failure's
// message, a note, the log tail the check run quotes -- is masked with every
// value this drive resolved before it is written or published (Review Focus
// 5).

const (
	// maxConcurrentSteps bounds one stage's steps in flight at once.
	maxConcurrentSteps = 16
	// stepGrace is how long past a run's ceiling the driver waits for the
	// runner to report a step before it stops waiting (pipeline_run_ceiling;
	// ruling R31b, stepDeadline). This must outlast both the cluster runner's
	// settling budget and the fleet's clone/packing plus Library transfer.
	stepGrace = 25 * time.Minute
	// executorCancelTimeout bounds Executor.Cancel.
	executorCancelTimeout = 30 * time.Second
	// githubCallTimeout bounds one call to GitHub the drive waits on: a token
	// mint, a compare, a check-run write.
	githubCallTimeout = 2 * time.Minute
	// treeReadTimeout bounds the tarball read, which carries a repository's
	// whole Go source.
	treeReadTimeout = 10 * time.Minute
	// workTemplatePrefix names a pipeline's work template: the run's
	// automationName is "pipeline:<pipeline name>".
	workTemplatePrefix = "pipeline:"
)

// ---------------------------------------------------------------------------
// The event
// ---------------------------------------------------------------------------

// HandleRunEvent is the driver's subscription to graph.node.created and
// graph.node.updated for v1:pipelines:run (app/integrations_pipelines_agent.go
// subscribes it on agent nodes). Every replica of every agent hears every
// run's every write -- the driver's own heartbeats included -- so all but two
// shapes return at once, having read nothing:
//
//   - a QUEUED run with no driver is claimed and driven, in a goroutine: the
//     claim is gated, so the agent replicas that hear the same event race for
//     it and exactly one wins;
//   - a run asking to be cancelled that THIS node drives is signalled.
//
// On a node that does not drive (Deps.Drive unset) it is a no-op.
func (i *Integration) HandleRunEvent(ev events.Event) {
	if i == nil {
		return
	}
	i.mu.RLock()
	drive := i.deps.Drive
	i.mu.RUnlock()
	if !drive {
		return
	}
	e, ok := runEventOf(ev)
	if !ok {
		return
	}
	if e.cancelRequested {
		if l := i.driving(e.id); l != nil {
			l.cancel()
		}
		return
	}
	if e.idOnly || (e.status == StatusQueued && e.driverNodeID == "") {
		// An id-only event carried no payload to decide from: the claim's
		// own fresh read decides, which costs one gated read and is cheaper
		// than a run nobody starts.
		i.spawnDrive(e.id)
	}
}

// runEvent is the slice of a run's event the driver decides on.
type runEvent struct {
	id              string
	status          string
	driverNodeID    string
	cancelRequested bool
	idOnly          bool
}

// runEventOf reads a run's graph event: the engine publishes the row's id and
// its payload, flattened at the top and whole under "payload".
func runEventOf(ev events.Event) (runEvent, bool) {
	if ev.Payload == nil {
		return runEvent{}, false
	}
	if topic := strings.TrimSpace(ev.Topic); topic != "" && !strings.HasSuffix(topic, "."+RunConcept) {
		return runEvent{}, false
	}
	id, _ := ev.Payload["id"].(string)
	if id = bareID(id); id == "" {
		return runEvent{}, false
	}
	fields, ok := ev.Payload["payload"].(map[string]any)
	if !ok {
		if _, flat := ev.Payload["status"]; !flat {
			return runEvent{id: id, idOnly: true}, true
		}
		fields = ev.Payload
	}
	return runEvent{
		id:              id,
		status:          rowString(fields, "status"),
		driverNodeID:    rowString(fields, "driverNodeId"),
		cancelRequested: rowBool(fields, "cancelRequested"),
	}, true
}

// ---------------------------------------------------------------------------
// Claim and drive
// ---------------------------------------------------------------------------

// cannotDrive names the port a driver lacks, or "" when it has all of them.
func (d Deps) cannotDrive() string {
	switch {
	case d.Store == nil:
		return "no store"
	case d.Gate == nil:
		return "no cross-replica gate"
	case strings.TrimSpace(d.NodeID) == "":
		return "no MEMQL_NODE_ID to hold a lease under"
	case d.Journal == nil:
		return "no work journal"
	case d.GitHub == nil:
		return "no GitHub port"
	}
	return ""
}

// claimAndDrive claims runID for this node and, when the claim is won,
// drives it to its conclusion. It returns when the drive does.
func (i *Integration) claimAndDrive(runID string) {
	runID = bareID(runID)
	d := i.snapshot()
	if !d.Drive || runID == "" {
		return
	}
	if missing := d.cannotDrive(); missing != "" {
		d.Logger.Warn("pipelines: this node is set to drive runs and cannot: "+missing,
			"component", "pipelinerun", logger.Subject(RunConcept, runID))
		return
	}
	l := i.reserve(runID)
	if l == nil {
		return // this node drives it already
	}
	defer i.release(l)

	ctx := context.Background()
	run, ok, err := i.claim(ctx, d, runID)
	if err != nil {
		d.Logger.Warn("pipelines: claiming a run failed; recovery tries again",
			"component", "pipelinerun", logger.Subject(RunConcept, runID), "error", err)
		return
	}
	if !ok {
		return // finished, or another replica's
	}
	dr := &runDriver{
		i: i, d: d, lease: l, runID: runID, run: run, claimID: run.DriverLeaseID,
		log: d.Logger.With("component", "pipelinerun", logger.Subject(RunConcept, runID), "node", d.NodeID),
	}
	switch dr.drive(ctx) {
	case driveConcluded:
		i.forgetAborts(runID)
	case driveAborted:
		i.noteAbort(runID, d.now())
	}
}

// driveOutcome is how a drive ended, for the node's recovery backoff.
type driveOutcome int

const (
	// driveConcluded: the run has its conclusion.
	driveConcluded driveOutcome = iota
	// driveAborted: a write or read the run cannot go on without failed; the
	// run stays claimed by this node for recovery to take up again.
	driveAborted
	// driveLost: another replica holds the run now.
	driveLost
)

// runDriver is one drive of one run, on the replica holding its lease.
type runDriver struct {
	i       *Integration
	d       Deps
	lease   *lease
	runID   string
	claimID string // immutable for this drive; a node name is not process ownership
	log     *slog.Logger

	// run is the row as this drive last wrote or read it. Only the drive's
	// own goroutine touches it; a step's goroutine reads facts instead.
	run          Run
	p            Pipeline
	havePipeline bool
	token        string
	installation int64

	workRun       atomic.Pointer[workjournal.Run]
	journalFailed atomic.Bool
	cleanupFailed atomic.Bool
	facts         requestFacts

	workflow *workflowProgram
	// computePlacement is the installed compute policy, read once per drive
	// (readPlan) for any plan with a command step and nil for one without. Its
	// capacity half serves a pool step; its retry limits serve every command step.
	// It is bound into each command step's recovery definition (definitionOf) and
	// copied into each command step's request (request).
	computePlacement *pipelines.ComputePlacementPolicy
	stages           []stageTracks
	tracks           []*stepTrack
	// abandonedInFlight: this drive ends steps a previous driver handed to
	// the runner without re-sending them, so the runner must be told to stop
	// them (reopenFromRows, conclude). The drive's goroutine only.
	abandonedInFlight bool

	execCtx    context.Context
	cancelExec context.CancelFunc

	maskMu sync.Mutex
	masks  []string

	// progressMu serializes progress (the run's stage table and its check run),
	// whoever asks: the workflow's reportProgress after a step settles, and
	// runStep after it records a retry. Each call snapshots the steps and then
	// writes GitHub, so two at once could land the older snapshot last and leave
	// the check run showing a step as it was. It lives on the driver, not on the
	// workflow host that used to own it, for that reason: the second caller is
	// not the host's.
	progressMu sync.Mutex

	timingsMu sync.Mutex
	observed  map[string]float64
}

// work is the run's work-journal handle; nil (every method a no-op) until the
// work run is opened.
func (dr *runDriver) work() *workjournal.Run { return dr.workRun.Load() }

// verdict is how a drive ends.
type verdict struct {
	conclusion string
	// refusal ended the run before a step ran (D9): written on the run row
	// and rendered as the check run's whole report.
	refusal *pipelines.Refusal
	// workCode and workMessage close the work run.
	workCode, workMessage string
	// outsideSteps: the run failed for a reason no step reported, so
	// workCode and workMessage are ALSO the run's own reason, written on the
	// row (Run.Failure) for the check run and the run page to state. Unset
	// where they only restate a failed step's report, which the step's own
	// row carries.
	outsideSteps bool
}

func refusedWith(r *pipelines.Refusal) verdict {
	return verdict{conclusion: ConclusionFailure, refusal: r, workCode: r.Code, workMessage: r.Detail}
}

// failedOutsideSteps is a failure no step's report explains: a workflow that
// erred after its steps, a required step blocked or never reported, a resumed
// plan that changed with nothing left to fail. Without it on the row, the
// check run read "Failed" over a stage table of Passed, and only the work
// run's journal said why (memql pull requests 6312, 6313 and 6318).
func failedOutsideSteps(code, message string) verdict {
	return verdict{conclusion: ConclusionFailure, workCode: code, workMessage: message, outsideSteps: true}
}

// drive takes the run from claimed to concluded. The heartbeat renews the
// lease beside it -- through the conclusion too, which calls GitHub and may
// take a while; a lease left to go stale there would let another replica take
// the run over mid-conclusion -- and a cancel stops the runner's in-flight
// work at once.
func (dr *runDriver) drive(ctx context.Context) driveOutcome {
	ctx, stopJournal := context.WithCancel(ctx)
	defer stopJournal()
	dr.guardJournal()
	dr.execCtx, dr.cancelExec = context.WithCancel(context.WithoutCancel(ctx))
	if dr.run.CancelRequested {
		dr.lease.cancel()
	}

	beatCtx, stopBeat := context.WithCancel(ctx)
	var beating sync.WaitGroup
	beating.Add(1)
	go func() {
		defer beating.Done()
		dr.heartbeat(beatCtx)
	}()
	watched := make(chan struct{})
	go func() {
		select {
		case <-watched:
		case <-dr.lease.stop:
			// A cancel reaches every step's Execute at once; a lost lease
			// leaves them running for the replica taking over.
			if dr.lease.isCancelled() {
				dr.cancelExec()
			}
		}
	}()

	outcome := driveAborted
	if v, ok := dr.steer(ctx); ok && dr.conclude(ctx, v) {
		outcome = driveConcluded
	}
	stopBeat()
	beating.Wait()
	close(watched)
	if dr.journalFailed.Load() || dr.cleanupFailed.Load() {
		return driveAborted
	}
	if dr.lease.isLost() {
		return driveLost
	}
	dr.cancelExec()
	return outcome
}

// cancelRunner tells the executor to stop every step of the run it still has
// in flight. A failure leaves the durable cancel request unfinished so another
// driver can retry it; a logged warning is not a confirmed cancellation.
func (dr *runDriver) cancelRunner() bool {
	exec := pipelines.CurrentExecutor()
	var err error
	if exec == nil {
		err = errors.New("no executor is available to confirm cancellation")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), executorCancelTimeout)
		err = exec.Cancel(ctx, dr.runID)
		cancel()
	}
	if err != nil {
		dr.log.Warn("pipelines: the runner did not confirm the run's cancel", "error", dr.mask(err.Error()))
		dr.cleanupFailed.Store(true)
		dr.lease.lose()
		return false
	}
	return true
}

// steer drives the run up to its verdict. ok is false when the drive must
// stop with nothing more written: the lease was lost, or a write the run
// cannot go on without failed -- either way the run stays claimed, and
// recovery takes it up again.
func (dr *runDriver) steer(ctx context.Context) (verdict, bool) {
	d := dr.d

	// The pipeline, read fresh: it may have been disconnected since the run
	// was opened.
	p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), dr.run.PipelineID)
	if err != nil {
		dr.abort("reading the run's pipeline", err)
		return verdict{}, false
	}
	if p != nil {
		dr.p, dr.havePipeline = *p, true
	}
	// A cancel is answered first: whatever else is true of the run, the
	// person asked it to stop.
	if dr.lease.isCancelled() {
		return verdict{conclusion: ConclusionCancelled, workMessage: "The run was cancelled."}, true
	}
	if p == nil {
		return refusedWith(pipelines.Refuse(pipelines.CodeDisconnected, "",
			"This run's pipeline no longer exists, so nothing can run it. Connect the source's pipeline again and re-run.")), true
	}
	requestedRelease := dr.run.Event == pipelines.EventRelease && dr.run.Trigger == TriggerManual
	if !p.Active() && dr.run.Status == StatusQueued && dr.run.Event != pipelines.EventManual && !requestedRelease {
		// A run already started runs to its end; one that has not, does not.
		return refusedWith(pipelines.Refuse(pipelines.CodeDisconnected, "",
			"This run's pipeline was disconnected before the run started, so it was not run. Reconnect the pipeline to run this commit again.")), true
	}

	// The grant: every token is minted through the owner's connection, which
	// proves it still reaches the repository. Bounded, and given up the moment
	// the run is cancelled or lost: a call to GitHub never holds a drive.
	tokenCtx, tokenDone := dr.stoppable(ctx, githubCallTimeout)
	token, installation, err := d.GitHub.InstallationToken(tokenCtx, p.CredentialID, p.OwnerUserID, p.Repository)
	tokenDone()
	switch {
	case dr.lease.isLost():
		return verdict{}, false
	case dr.lease.isCancelled():
		return verdict{conclusion: ConclusionCancelled, workMessage: "The run was cancelled."}, true
	case err != nil:
		return refusedWith(grantRefusal(p.Repository, err)), true
	}
	dr.token, dr.installation = token, installation
	if dr.installation == 0 {
		dr.installation = p.InstallationID
	}

	// Started.
	if dr.run.Status == StatusQueued {
		if err := dr.writeRun(ctx, RunPatch{Status: ptr(StatusInProgress), StartedAt: ptr(d.now())}); err != nil {
			dr.abort("moving the run to in_progress", err)
			return verdict{}, false
		}
		dr.publish(ctx) // "Preparing the run"
	}

	// The plan. A read the drive stopped answers a refusal that is only the
	// stop's echo, so the stop is asked about first.
	plan, refusal := dr.readPlan(ctx)
	switch {
	case dr.lease.isLost():
		return verdict{}, false
	case dr.lease.isCancelled():
		return verdict{conclusion: ConclusionCancelled, workMessage: "The run was cancelled."}, true
	case refusal != nil:
		return refusedWith(refusal), true
	}

	if len(plan.Stages) == 0 {
		// A changed DSL selector must not erase an already-journaled plan on
		// recovery (or turn a failed-only rerun into a passing empty run).
		return dr.emptyPlanVerdict(), true
	}

	// The work run.
	stop, ok := dr.openWork(ctx, plan)
	if !ok {
		return verdict{}, false
	}
	if stop != nil {
		return *stop, true
	}
	dr.publish(ctx)

	workflowErr := dr.execute(ctx)
	if dr.cleanupFailed.Load() && !dr.lease.isCancelled() {
		// The workflow waits for its active branches. Keep renewing ownership
		// until their results are journaled, particularly Fleet commands whose
		// outcome cannot be recovered from a cluster Job. No new work is
		// admitted while draining, and recovery must still confirm cleanup.
		dr.lease.lose()
	}
	if dr.lease.isLost() {
		return verdict{}, false
	}
	if workflowErr != nil && !dr.lease.isCancelled() {
		// Whatever the steps did: an independent review that reached no
		// model, an operation the workflow called wrongly. Every step may
		// have passed, so the reason is the run's own.
		return failedOutsideSteps(pipelines.CodeExecutorError, "Pipeline workflow failed: "+dr.mask(workflowErr.Error())), true
	}
	return dr.verdictOfSteps(), true
}

func (dr *runDriver) emptyPlanVerdict() verdict {
	if strings.TrimSpace(dr.run.WorkRunID) != "" || dr.run.RerunFailedOnly {
		return failedOutsideSteps(pipelines.CodeNodeLost,
			"The pipeline DSL selected no stages for a run with an existing work plan, so its original checks cannot be proven. Re-run the pipeline.")
	}
	// No stage applies to a fresh run: success with nothing to run, and no
	// work goal for nothing.
	return verdict{conclusion: ConclusionSuccess}
}

// abort logs why a drive stopped short. A lost lease was logged where it was
// found.
func (dr *runDriver) abort(what string, err error) {
	if driveStopped(err) {
		return
	}
	dr.log.Warn("pipelines: the drive stopped "+what+"; the run stays claimed, and recovery takes it up again",
		"error", dr.mask(err.Error()))
}

// ---------------------------------------------------------------------------
// The work run
// ---------------------------------------------------------------------------

// openWork opens the plan's work run -- one goal, one run, every step queued
// -- or, when the run already has one, reopens it and resumes. It answers a
// verdict when the run cannot go on as planned, and false when the drive must
// stop.
func (dr *runDriver) openWork(ctx context.Context, plan pipelines.Plan) (*verdict, bool) {
	d := dr.d
	dr.buildTracks(plan)

	if strings.TrimSpace(dr.run.WorkRunID) != "" {
		rows, err := d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), dr.run.WorkRunID)
		if err != nil {
			dr.abort("reading the work run's steps to resume it", err)
			return nil, false
		}
		if len(rows) > 0 {
			if why := dr.resume(rows); why != "" {
				if !dr.reopenFromRows(rows) {
					return nil, false
				}
				v := dr.diverged(ctx, why)
				return &v, true
			}
			// Declared AFTER resume, so a step's call names the slice the
			// rows gave it back: a re-sent step's intent restates its row,
			// it does not overwrite it with a slice recomputed since.
			w := d.Journal.Reopen(dr.run.OwnerUserID, dr.run.WorkGoalID, dr.run.WorkRunID, dr.decls(), dr.run.StartedAt)
			if w == nil {
				dr.abort("reopening the run's work run", errors.New("the run row names no work goal to reopen"))
				return nil, false
			}
			dr.workRun.Store(w)
			dr.setFacts()
			clean := true
			for _, track := range dr.tracks {
				// Every attempt whose end the rows record is acknowledged again
				// before this drive starts anything: a finished step's last, and
				// the attempt a step between two attempts last ended. The replica
				// that recorded the outcome may have lost the run before it
				// confirmed that attempt's Job, Secret and pods gone, or failed to
				// confirm it, and a cluster attempt's acknowledgement returns only
				// once they are. So the step's next attempt is never sent beside a
				// Job of its last that may still start its command or hold its
				// slot, and an unconfirmed one stops this drive (below) with the
				// row as it was, for recovery.
				if track.step.Kind == pipelines.StepCommand && (track.finished() || track.betweenAttempts) {
					if !dr.confirmReceiptCleanup(ctx, pipelines.CurrentExecutor(), dr.request(track, nil)) {
						clean = false
						if dr.lease.isLost() {
							break
						}
					}
				}
			}
			if !clean {
				// One unresolved attempt must not keep every later finished
				// Job occupying capacity. Reconcile all durable receipts while
				// we hold the lease, then stop before executing or concluding.
				dr.lease.lose()
				return nil, false
			}
			return nil, true
		}
		// A work run with no step rows never queued its steps: opening it
		// again writes the same ids, and nothing has run to lose.
	}

	// A FAILED-ONLY RE-RUN carries what its original passed (epic
	// memql#5479). Here and only here: on a resume the carried steps are
	// already in the rows, each a skip the declarations below recorded, and
	// reading the original again could only disagree with what this run
	// began as.
	if dr.run.RerunFailedOnly && !dr.carryFromOriginal(ctx) {
		return nil, false
	}
	if !dr.reuseVerification(ctx) {
		return nil, false
	}

	work := workjournal.Work{
		OwnerUserID: dr.run.OwnerUserID,
		Template:    workTemplatePrefix + dr.p.Name,
		Statement:   fmt.Sprintf("Run %s on %s (%s)", dr.p.Name, shortSHA(dr.run.SHA), dr.run.Event),
		// The run key, scoped to the pipeline as RunIDFor scopes the run row:
		// a disconnected predecessor's run of the same key keeps its own work.
		GoalKey: bareID(dr.p.ID) + "|" + dr.run.RunKey,
		RunKey:  strconv.Itoa(max(dr.run.Attempt, 1)),
		Input: map[string]any{
			"pipelineRunId": bareID(dr.run.ID),
			"pipelineId":    bareID(dr.p.ID),
			"repository":    dr.p.Repository,
			"sha":           dr.run.SHA,
			"mode":          string(dr.run.Mode),
			"event":         string(dr.run.Event),
			"attempt":       max(dr.run.Attempt, 1),
		},
		Steps:        dr.decls(),
		QueueSteps:   true,
		RequestedVia: "pipeline",
		TriggeredBy:  pipelines.WorkTriggerPrefix + string(dr.run.Mode),
	}
	// The work is NAMED on the run row before it is opened: a replica that
	// stops between the two leaves a row naming work that may not exist yet,
	// which a resume opens (no step rows: Begin again, the same ids), and
	// never work that nothing names -- a pipeline's work run is its runner's
	// own, and no sweep would ever close it.
	goalID, runID, err := workjournal.IDs(work)
	if err != nil {
		dr.abort("naming the run's work", err)
		return nil, false
	}
	if err := dr.writeRun(ctx, RunPatch{
		WorkRunID: ptr(runID), WorkGoalID: ptr(goalID), Stages: ptr(dr.stageSummaries()),
	}); err != nil {
		dr.abort("recording the run's work run", err)
		return nil, false
	}
	if !dr.stillHolds(ctx) {
		return nil, false
	}
	w, err := d.Journal.Begin(ctx, work)
	if err != nil || w == nil {
		if err == nil {
			err = errors.New("the journal opened nothing")
		}
		dr.abort("opening the run's work goal", err)
		return nil, false
	}
	dr.workRun.Store(w)
	dr.setFacts()
	return nil, true
}

// carryFromOriginal reads the attempt this one re-runs, and its steps, and
// carries every step it passed (carryPassed). It answers false when the drive
// must stop: a read that did not answer is not "nothing passed", because
// running everything again is not what was asked for. An original nobody can
// read any more -- retention retired it -- carries nothing, and the run runs
// every step, which is the honest reading of "re-run what did not pass" when
// what passed can no longer be shown.
func (dr *runDriver) carryFromOriginal(ctx context.Context) bool {
	d := dr.d
	originalID := strings.TrimSpace(dr.run.RerunOf)
	if originalID == "" {
		return true
	}
	fresh := memql.ContextWithFreshRead(ctx)
	original, err := d.Store.RunByID(fresh, originalID)
	if err != nil {
		dr.abort("reading the attempt this run re-runs", err)
		return false
	}
	if original == nil || strings.TrimSpace(original.WorkRunID) == "" {
		dr.log.Warn("pipelines: the attempt a failed-only re-run re-runs is gone; every step runs",
			"rerunOf", originalID)
		return true
	}
	rows, err := d.Store.WorkSteps(fresh, original.WorkRunID)
	if err != nil {
		dr.abort("reading the steps of the attempt this run re-runs", err)
		return false
	}
	carryPassed(dr.tracks, rows, max(original.Attempt, 1))
	return true
}

// carryPassed marks a planned step skipped pipeline_passed_earlier when the
// attempt it re-runs PASSED it with the same package slice, or had carried it
// already -- keeping the attempt it first passed in. A shard whose slice moved
// since (the timing table learned between the attempts) runs again: a pass
// over other packages is not a pass of these. A failed, cancelled or never
// reached step runs, and a step the plan skips keeps the plan's reason.
//
// A NOTIFY STEP IS NEVER CARRIED. What it delivered announced the attempt it
// ran in; this attempt has an outcome of its own -- most often, that the
// pipeline recovered -- and a carried notification would leave it unsaid.
func carryPassed(tracks []*stepTrack, prior []WorkStep, priorAttempt int) {
	// Report publications bind the producing run, so a new attempt must produce
	// its own scanner artifacts instead of carrying a skip from an older run.
	producers := map[string]bool{}
	for _, t := range tracks {
		if t.step.RepositoryChange != nil {
			producers[t.step.RepositoryChange.Step] = true
		}
		if t.step.SARIF != nil {
			producers[t.step.SARIF.Step] = true
		}
	}
	byKey := make(map[string]WorkStep, len(prior))
	for _, row := range prior {
		if row.Key != "" {
			byKey[row.Key] = row
		}
	}
	for _, t := range tracks {
		row, ok := byKey[t.step.Key]
		if !ok || t.step.Skip != nil || t.step.Kind != pipelines.StepCommand || producers[t.step.Key] {
			continue
		}
		passed := row.Status == WorkStepDone
		carried := row.Status == WorkStepSkipped && row.Skip != nil && row.Skip.Code == pipelines.CodePassedEarlier
		if !passed && !carried {
			continue
		}
		if t.definition == "" || row.DefinitionFingerprint != t.definition {
			continue
		}
		if !slices.Equal(row.Packages, t.step.Packages) {
			continue
		}
		reason := fmt.Sprintf("Passed in attempt %d.", priorAttempt)
		if carried && strings.TrimSpace(row.Skip.Reason) != "" {
			reason = row.Skip.Reason
		}
		t.step.Skip = &pipelines.Skip{Code: pipelines.CodePassedEarlier, Reason: reason}
	}
}

// decls are the plan's steps as the journal declares them, in plan order: an
// exec step each, deterministic, waiting on the previous stage's steps. The
// call names the manifest step and carries what the plan DECIDED about it
// that a later read could decide differently -- the package slice a step was
// given, and the skip it was planned with -- names and sentences only, never
// a secret value, so a resumed driver carries out the plan the run began with
// rather than one computed from a timing table or a compare that moved since.
func (dr *runDriver) decls() []workjournal.StepDecl {
	out := make([]workjournal.StepDecl, 0, len(dr.tracks))
	for _, t := range dr.tracks {
		call := map[string]any{"construct": "pipeline", "name": t.step.Name, "stage": t.step.Stage, "pipelineKind": string(t.step.Kind)}
		call["definitionFingerprint"] = t.definition
		call["displayTitle"] = strings.ReplaceAll(t.step.Stage+" / "+t.step.Name, "-", " ")
		call["summary"] = string(t.step.Kind)
		if t.verification != "" {
			call["verificationFingerprint"] = t.verification
		}
		if t.evidenceRunID != "" {
			call["evidenceRunId"] = t.evidenceRunID
		}
		if len(t.step.Packages) > 0 {
			call["packages"] = slices.Clone(t.step.Packages)
		}
		if t.step.Skip != nil {
			call["skip"] = map[string]any{"code": t.step.Skip.Code, "reason": t.step.Skip.Reason}
		}
		out = append(out, workjournal.StepDecl{
			Key:       t.step.Key,
			Kind:      workjournal.KindDeterministic,
			StepType:  "exec",
			DependsOn: slices.Clone(t.step.DependsOn),
			Call:      call,
		})
	}
	return out
}

// resume lays the step rows a previous driver wrote over the plan read again.
// Every step is carried out as its ROW says the run planned it -- the slice it
// was given, and whether it was a skip -- from where its row says its attempts
// stand. It answers why the run cannot be resumed, or "": a step the rows hold
// and the plan does not is work the run began and could no longer account for.
//
// A STEP'S ROW IS IN ONE OF THREE STATES, and each decides what this drive
// does with the step (spec 2026-10-28-pipeline-fleet-first-retries-failover-
// design.md, section 3):
//
//   - FINISHED: the receipt is kept, and the step is not run again.
//   - ATTEMPT N BEGUN, NO RECORD OF IT: the row is running and its history
//     holds attempts 1 to N-1 only. Attempt N was handed to the runner, which
//     may still be at it, so it is sent again as a RECOVERY of attempt N
//     (sent: RecoverOnly), placed where it was sent -- even when the plan read
//     now would skip the step, because its outcome is the run's. The runner
//     re-attaches to attempt N's Job by its name; a machine holds no Job to
//     re-attach to and answers the outcome unknown, so a repeatable step goes
//     on to N+1 and any other step fails pipeline_execution_uncertain
//     (chooseNext).
//   - ATTEMPT N RECORDED, N+1 NOT BEGUN: the row is still running (a step with
//     an attempt to come never reads finished), its history ends with attempt
//     N's record, and its binding says when N+1 may start. Attempt N is not sent
//     again in any form: its end is recorded, and a recovery of it would find
//     its Job answered and released, read the outcome as unknown, and end a
//     step whose next attempt is due. The step continues at N+1
//     (betweenAttempts, runStep's continueAttempts), once attempt N's resources
//     are confirmed gone again (openWork) -- the replica that recorded the
//     outcome may have stopped before it confirmed them -- and once the row's
//     nextAttemptAt has come.
//
// A pending row never began, and its step runs from attempt 1.
//
// WHAT SURVIVES A REPLICA CHANGE is the row, and only the row: the attempt
// number, the record of every attempt that ended, and when the next is due.
// Nothing in the old replica's memory is needed. Each attempt's placement is
// recomputed from the records before it (placementOf), never stored, and is
// exactly the one the old replica chose because the recovery definition
// compared below binds both the engine revision and the run's captured retry
// policy (definitionOf): a replica that would decide under other rules diverges
// instead. The history is loaded into the track (StepState.Attempts) for every
// row, so every write this drive makes of the step restates it: the binding is
// replaced whole on each write, and a write without the history would erase the
// earlier records or keep a stale nextAttemptAt. A history that does not lead
// to the row's attempt (attemptsOfRow) cannot say which attempt to send or
// where, and the run diverges rather than guess.
func (dr *runDriver) resume(rows []WorkStep) string {
	stored := make(map[string]WorkStep, len(rows))
	for _, row := range rows {
		if row.Key != "" {
			if _, duplicate := stored[row.Key]; duplicate {
				return "the journal contains duplicate step " + row.Key
			}
			stored[row.Key] = row
		}
	}
	planned := make(map[string]bool, len(dr.tracks))
	for _, t := range dr.tracks {
		planned[t.step.Key] = true
	}
	var missing []string
	for key := range stored {
		if !planned[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Sprintf("the plan read again at %s no longer holds %s", shortSHA(dr.run.SHA), strings.Join(missing, ", "))
	}
	for index, t := range dr.tracks {
		row, ok := stored[t.step.Key]
		if !ok {
			return "the journal has no definition for step " + t.step.Key
		}
		t.attempt = max(row.Attempt, 1)
		t.sent, t.betweenAttempts, t.nextAttemptAt, t.placement = false, false, time.Time{}, nil
		t.step.Packages = slices.Clone(row.Packages)
		t.step.Skip = nil
		if row.Skip != nil {
			skip := *row.Skip
			t.step.Skip = &skip
		}
		t.definition = dr.definitionOf(t.step)
		if row.Seq != index || t.definition == "" || row.DefinitionFingerprint != t.definition {
			return "the execution definition or order changed for step " + t.step.Key
		}
		records, _ := attemptsFromBinding(row.Binding)
		t.setAttempts(records)
		switch {
		case row.Finished():
			t.set(stateFromRow(t.snapshot(), row))
		case row.Status == WorkStepRunning:
			if why := dr.resumeAttempts(t, row); why != "" {
				return why
			}
		}
	}
	return ""
}

// resumeAttempts lays a running row's attempts over its step's track (resume):
// the attempt the row names, sent again as a recovery when nothing records its
// end, or continued from when its end is recorded; and where it was sent.
func (dr *runDriver) resumeAttempts(t *stepTrack, row WorkStep) string {
	at, ok := attemptsOfRow(row)
	if !ok {
		return fmt.Sprintf("the attempts recorded for step %s do not lead to its attempt %d", t.step.Key, t.attempt)
	}
	before := at.records
	if at.ended {
		before = at.records[:len(at.records)-1]
	}
	placement, ok := placementOf(t.step, dr.retryPolicy(), before)
	if !ok {
		return fmt.Sprintf("the attempts recorded for step %s are not ones this run's retry rules make", t.step.Key)
	}
	t.placement = placement
	t.sent = !at.ended
	t.betweenAttempts = at.ended
	t.nextAttemptAt = at.next
	return ""
}

// diverged ends a resumed run whose plan, read again, is not the plan its
// work began: the steps are the ROWS' (reopenFromRows), and every one without
// a receipt fails pipeline_node_lost -- the driver that knew the plan is gone
// -- so the run fails rather than report a pass for work it can no longer
// account for.
func (dr *runDriver) diverged(ctx context.Context, why string) verdict {
	message := "The agent that was driving this run stopped, and " + why +
		", so the run cannot be resumed as it was planned. Re-run it."
	dr.log.Warn("pipelines: a resumed run's plan is not the plan its work began; its unfinished steps fail",
		"why", why)
	failed := 0
	for _, t := range dr.tracks {
		if !t.finished() {
			dr.settle(ctx, t, failReceipt(pipelines.CodeNodeLost, message))
			failed++
		}
	}
	if failed == 0 {
		// Every step had finished, so no step carries the message.
		return failedOutsideSteps(pipelines.CodeNodeLost, message)
	}
	return verdict{conclusion: ConclusionFailure, workCode: pipelines.CodeNodeLost, workMessage: message}
}

// adoptRows makes the work run's step ROWS this drive's steps, in their plan
// order: what a drive reports and settles when it has no plan of its own to
// lay over them.
//
// Each step carries what its row says of its attempts -- its history, and
// whether it was in flight or between two attempts -- because these steps are
// settled without being run (diverged, settleStoredWork), and the receipt that
// settles one must restate the history and name no next attempt: the binding is
// replaced whole, and a receipt that named none would keep the row's
// nextAttemptAt for an attempt that will never start.
func (dr *runDriver) adoptRows(rows []WorkStep) {
	sorted := slices.Clone(rows)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	dr.tracks, dr.stages = nil, nil
	at := map[string]int{}
	for _, row := range sorted {
		stage, name := row.Stage, row.Name
		if stage == "" {
			stage, _, _ = strings.Cut(row.Key, ".")
		}
		t := &stepTrack{
			step:       pipelines.Step{Key: row.Key, Stage: stage, Name: name, Kind: pipelines.StepCommand},
			definition: row.DefinitionFingerprint,
			attempt:    max(row.Attempt, 1),
			state:      StepState{Key: row.Key, Stage: stage, Name: name, Status: StepPending},
		}
		records, _ := attemptsFromBinding(row.Binding)
		t.setAttempts(records)
		if row.Status == WorkStepRunning {
			// A history that does not lead to the row's attempt reads as in
			// flight: the step is settled, never run, so all that matters is
			// that the runner is told to stop it (reopenFromRows).
			rowAt, ok := attemptsOfRow(row)
			t.betweenAttempts = ok && rowAt.ended
			t.sent = !t.betweenAttempts
		}
		if row.Finished() {
			t.set(stateFromRow(t.snapshot(), row))
		}
		dr.tracks = append(dr.tracks, t)
		i, ok := at[stage]
		if !ok {
			i = len(dr.stages)
			at[stage] = i
			dr.stages = append(dr.stages, stageTracks{name: stage})
		}
		dr.stages[i].tracks = append(dr.stages[i].tracks, t)
	}
}

// settleStoredWork closes the work a resumed run's predecessor opened when
// this drive ends the run before reading its plan again -- a cancel, or a
// refusal (a revoked grant, a tree GitHub would not hand over): the work run
// reopened from its rows, every step without a receipt settled with rec, so
// the work run closes with the run instead of standing open with nobody to
// close it (a pipeline's work run is its runner's own; no sweep judges it).
// It answers false when the rows could not be read and the run must stay
// unfinished, for recovery to conclude it again.
func (dr *runDriver) settleStoredWork(ctx context.Context, rec receipt) bool {
	rows, err := dr.d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), dr.run.WorkRunID)
	if err != nil {
		dr.abort("reading the work run's steps to close them", err)
		return false
	}
	if !dr.reopenFromRows(rows) {
		return false
	}
	for _, t := range dr.tracks {
		if !t.finished() {
			dr.settle(ctx, t, rec)
		}
	}
	return true
}

// reopenFromRows makes the work run's step rows this drive's steps
// (adoptRows) and reopens the work run with the ROWS' own declarations, in
// their order: an intent written through them restates each step's type,
// kind and place, and leaves its call and dependencies as the plan that
// queued it wrote them -- a declaration the journal does not hold would write
// the default type over the row's. Its callers settle every unfinished step
// without re-sending it, so a step the runner was handed and never answered
// for is ABANDONED here, and the runner is told to stop the run's work
// (conclude). So is a step left between two attempts: this drive confirmed
// nothing of its last attempt's resources, which the replica that recorded the
// outcome may not have confirmed gone either, and stopping the run's work is
// what releases them.
func (dr *runDriver) reopenFromRows(rows []WorkStep) bool {
	dr.adoptRows(rows)
	decls := make([]workjournal.StepDecl, 0, len(dr.tracks))
	for _, t := range dr.tracks {
		decls = append(decls, workjournal.StepDecl{Key: t.step.Key, Kind: workjournal.KindDeterministic, StepType: "exec"})
		if (t.sent || t.betweenAttempts) && !t.finished() {
			dr.abandonedInFlight = true
		}
	}
	w := dr.d.Journal.Reopen(dr.run.OwnerUserID, dr.run.WorkGoalID, dr.run.WorkRunID, decls, dr.run.StartedAt)
	if w == nil {
		dr.abort("reopening the run's work run", errors.New("the run row names no work goal to reopen"))
		return false
	}
	dr.workRun.Store(w)
	return true
}

// stateFromRow is a step's state as its row's receipt says it ended -- its
// artifacts too, so a notify stage of a resumed run lists the files of steps
// its predecessor ran.
func stateFromRow(s StepState, row WorkStep) StepState {
	s.Code, s.DurationMs = row.ErrorCode, row.DurationMs
	s.Message = row.ErrorMessage
	if s.Message == "" {
		s.Message = row.Reason
	}
	s.ArtifactFileIDs = slices.Clone(row.ArtifactFileIDs)
	switch row.Status {
	case WorkStepDone:
		s.Status = StepSucceeded
	case WorkStepFailed:
		s.Status = StepFailed
	case WorkStepSkipped:
		s.Status = StepSkipped
	case WorkStepCancelled:
		s.Status = StepCancelled
	}
	return s
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

// stageTracks is one planned stage's steps.
type stageTracks struct {
	name   string
	tracks []*stepTrack
}

// stepTrack is one compiled step as this drive follows it.
type stepTrack struct {
	step pipelines.Step
	// attempt is the attempt in flight, or about to be sent: 1 for a step this
	// drive planned, the row's for one it resumed, raised by runStep before each
	// retry. Only the step's goroutine moves it.
	attempt       int
	definition    string
	verification  string
	evidenceRunID string
	// sent: the rows say the attempt t names is running and record no outcome
	// for it -- a previous driver handed it to the runner, which may still be
	// at it. Re-sent as a recovery of that same attempt (RecoverOnly) when the
	// run goes on; when the run ends instead, the runner is told to stop it. A
	// retry this drive starts is a new attempt, never a recovery, so runStep
	// clears it before one.
	sent bool
	// betweenAttempts: the rows say the attempt t names has ENDED with another
	// to come -- its record is the last of the step's history (StepState.
	// Attempts) and the row says when the next may start (nextAttemptAt, the
	// zero time when it names none). A previous driver left the step between
	// two attempts. runStep sends no copy of this attempt, not even a recovery:
	// it continues at the next one (continueAttempts), which clears both. Set
	// only by resume and adoptRows, and only where the history ends at t's
	// attempt (attemptsOfRow), so that history is never empty.
	betweenAttempts bool
	nextAttemptAt   time.Time
	// placement is where the attempt in flight was sent (chooseNext): nil for
	// a first attempt, which goes where its manifest placed it. For a step a
	// previous driver began, resume recomputes it from the row's history.
	// Only the step's goroutine touches it.
	placement *pipelines.AttemptPlacement
	// handle is the journal handle of the attempt in flight, once its intent
	// is written: each attempt's intent returns its own (StepAttempt), and the
	// receipt finishes the last one. Only the goroutine settling the step
	// touches it.
	handle *workjournal.Step

	mu    sync.Mutex
	state StepState
}

func (t *stepTrack) snapshot() StepState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *stepTrack) set(s StepState) {
	t.mu.Lock()
	t.state = s
	t.mu.Unlock()
}

// setAttempts records the step's attempt history in its state, a copy of
// history's.
//
// IT ENDS THE WORD THAT ANOTHER ATTEMPT FOLLOWS (Retrying, RetryAfter). That
// word was set for the outcome of the attempt before the one whose record has
// just arrived, and runStep leaves it up while that next attempt runs; once the
// next attempt's record is in the history the word is about an outcome nobody
// is waiting on any more. Left up, a progress another step publishes before
// this step's receipt (or before the outcome that sends it round again is
// durable) says attempt N failed and attempt N+1 follows, for a step that may
// be about to pass. The order "outcome durable, then setRetrying" is unchanged:
// the word is set again, about the new record, only after the next Retrying is
// written. Covered by TestARecordedAttemptEndsTheWordThatAnotherFollows.
func (t *stepTrack) setAttempts(history []attemptRecord) {
	t.mu.Lock()
	t.state.Attempts = slices.Clone(history)
	t.state.Retrying, t.state.RetryAfter = false, 0
	t.mu.Unlock()
}

// setRetrying records in the step's state that another attempt follows, after
// the wait the driver scheduled: what the check run reads (StepState.Retrying).
func (t *stepTrack) setRetrying(after time.Duration) {
	t.mu.Lock()
	t.state.Retrying, t.state.RetryAfter = true, after
	t.mu.Unlock()
}

// finished reports whether the step has ended, one way or another.
func (t *stepTrack) finished() bool {
	switch t.snapshot().Status {
	case StepSucceeded, StepFailed, StepRefused, StepCancelled, StepSkipped:
		return true
	}
	return false
}

func (dr *runDriver) buildTracks(plan pipelines.Plan) {
	dr.tracks, dr.stages = nil, nil
	for _, stage := range plan.Stages {
		st := stageTracks{name: stage.Name}
		for _, step := range stage.Steps {
			t := &stepTrack{
				step: step, attempt: 1,
				definition:   dr.definitionOf(step),
				verification: dr.verificationOf(step),
				state:        StepState{Key: step.Key, Stage: step.Stage, Name: step.Name, Status: StepPending},
			}
			st.tracks = append(st.tracks, t)
			dr.tracks = append(dr.tracks, t)
		}
		dr.stages = append(dr.stages, st)
	}
}

// runStep takes one step from pending to its receipt, through as many
// attempts as its retry rules give it (attempts.go; spec
// 2026-10-28-pipeline-fleet-first-retries-failover-design.md, section 3).
//
// EACH ATTEMPT IS JOURNALED BEFORE IT STARTS AND ITS OUTCOME BEFORE THE NEXT
// ONE STARTS, as versions of the step's one row: the intent of attempt N
// (StepAttempt: running, attempt N, the records of 1 to N-1), then, when an
// attempt follows, N's outcome (Retrying: still running, N's record added,
// nextAttemptAt), and only then the next intent. So a replica that takes the
// run over finds on the row which attempt was in flight or which one is due,
// and never repeats an attempt whose end was recorded.
//
// BETWEEN ATTEMPTS THE ROW NEVER READS FINISHED: the stage workflow, the check
// run and a takeover all treat a failed step as final, and the step has not
// ended. Its state in memory stays running too.
//
// THE ATTEMPT'S RESOURCES ARE ACKNOWLEDGED BEFORE THE NEXT ONE IS SENT: for a
// cluster attempt the acknowledgement returns only once its Job, Secret and
// pods are gone, so no waiting retry holds a cluster slot and two Jobs of one
// step never run at once. It follows the outcome's record because the
// acknowledgement contract releases resources only behind a durable record of
// how they ended (pipelines.ReceiptAcknowledger).
//
// A STEP A PREVIOUS DRIVER LEFT BETWEEN TWO ATTEMPTS (betweenAttempts, resume)
// enters the loop where that driver stopped, at the wait for the next attempt
// (continueAttempts): no intent of the attempt that ended is written again, and
// no copy of it is sent. Its resources were confirmed gone before this drive
// started anything (openWork).
//
// THE LOOP STOPS, and the step ends with its last attempt's receipt (or, when
// that attempt only found the fleet full, the failure before it:
// endingReceipt), when the attempt passed or ended for good (its class), when
// chooseNext gives the step no more attempts (the policy's limits,
// MaxStepAttempts), or when the next attempt could not start with a second of
// the run's ceiling left -- a wait into the ceiling is never begun, and one the
// process slept past starts nothing. A cancel between attempts settles the step
// cancelled. A lost lease, an intent or outcome the journal refused, or a
// cleanup that could not be confirmed stops the drive with nothing more
// written, and the run stays unfinished for recovery. So does the end of the
// workflow call that runs the step, during a wait: nothing more is written
// (beginNextAttempt).
//
// ONE WRITER: every journal write of the step, every attempt's included, is
// made here, on the step's own goroutine. The workflow's other writers of a
// step (its skip, the cancel sweep after it returns) wait for this to return:
// the step's lock and the workflow's wait for its branches.
func (dr *runDriver) runStep(ctx context.Context, t *stepTrack) {
	if dr.lease.isLost() || dr.lease.isCancelled() || dr.cleanupFailed.Load() {
		return
	}
	step := t.step
	switch {
	case step.Skip != nil:
		dr.settle(ctx, t, skipReceipt(step.Skip.Code, step.Skip.Reason))
		return
	case step.Kind == pipelines.StepRepositoryChange:
		dr.runRepositoryChange(ctx, t)
		return
	case step.Kind == pipelines.StepSARIF:
		dr.runSARIFReport(ctx, t)
		return
	case step.Kind == pipelines.StepNotify:
		// The driver delivers a notification itself, over the outbound path
		// (notify.go): a notify step is never the step runner's.
		dr.runNotify(ctx, t)
		return
	}

	// The intent, before anything the step does -- unless the rows left the step
	// between two attempts: the attempt t names has ended and its outcome is on
	// the row, so nothing begins until the next attempt, whose intent is the
	// step's next write. Writing this attempt's intent again would read as the
	// attempt begun once more, and would drop the row's nextAttemptAt. With no
	// write here there is no bare ownership read either: one that failed for a
	// moment would leave the step unreported; the next attempt's intent, made
	// under the journal's write guard, is the ownership check.
	resumed := t.betweenAttempts
	if !resumed && (!dr.stillHolds(ctx) || !dr.recordIntent(ctx, t)) {
		return
	}
	running := t.snapshot()
	running.Status = StepRunning
	t.set(running)

	secrets, failure := dr.resolveSecrets(ctx, step)
	if failure != nil {
		dr.settle(ctx, t, failReceipt(failure.Code, failure.Message))
		return
	}
	exec := pipelines.CurrentExecutor()
	if exec == nil {
		dr.settle(ctx, t, failReceipt(pipelines.CodeRunnerUnavailable,
			"This cluster has no runner registered to execute steps, so the step did not run. Nothing in the repository is wrong."))
		return
	}
	switch {
	case dr.lease.isLost():
		return
	case dr.lease.isCancelled():
		// Asked to stop while its secrets resolved: never handed over.
		dr.settle(ctx, t, cancelReceipt())
		return
	}

	ceiling := pipelines.ParseRunCeiling(os.Getenv(pipelines.EnvRunCeiling))
	ceilingAt := dr.runCeilingAt(ceiling)
	policy := dr.retryPolicy()
	if resumed && !dr.continueAttempts(ctx, t, policy, ceilingAt) {
		return
	}
	for {
		req := dr.request(t, secrets)
		began := dr.d.now()
		end := dr.execStep(exec, req, dr.stepDeadline(ceiling))
		if end.kind == endLost {
			return // the attempt is the replica taking the run over's to re-attach to
		}
		rec := dr.attemptReceipt(end, ceiling, len(step.Artifacts) > 0)
		history := append(slices.Clone(t.snapshot().Attempts), dr.attemptOf(req, end, rec, began))
		t.setAttempts(history)

		next, nextAt, retry := dr.nextAttempt(step, policy, end, rec, history, ceilingAt)
		if !retry {
			// The step's receipt: this attempt's, restating the whole history
			// (settle) -- or, when this attempt only found the fleet full, the
			// failure before it (endingReceipt). A runner's answer releases this
			// attempt's resources once recorded; a timeout, an error or a cancel
			// leaves them to the run's cancel.
			if dr.settle(ctx, t, dr.endingReceipt(step, policy, history, rec)) && end.kind == endResult {
				dr.acknowledgeReceipt(ctx, exec, req)
			}
			return
		}

		// The attempt's outcome, durable before anything of the next: the step
		// is still running, its history holds this attempt, and the row says
		// when the next may start.
		if err := t.handle.Retrying(ctx, attemptsBinding(nil, history, nextAt)); err != nil {
			dr.journalUnavailable(step.Key, err)
			return
		}
		dr.log.Info("pipelines: a step's attempt failed and the step is tried again",
			"step", step.Key, "attempt", t.attempt, "class", string(history[len(history)-1].Class),
			"code", history[len(history)-1].Code, "next", describePlacement(next), "nextAttemptAt", nextAt)
		// The check run says so now. Nothing else would for up to minutes: the
		// workflow publishes progress when a step settles, and this one has not, so a
		// person reading the check saw the step as it stood before its first attempt
		// until the wait and the attempt after it were over. It is said only here,
		// once the outcome is DURABLE (a Retrying the journal refused returned above,
		// so the check run never promises a retry the row does not hold), and before
		// the cleanup and the wait, so GitHub's latency comes out of the wait rather
		// than adding to it. A failover to another machine has no wait, so for it the
		// publish delays the next attempt directly, by one GitHub round trip. The
		// bound is githubCallTimeout (2 minutes) PER PUBLISH CALL, not per attempt:
		// progress is serialized by progressMu, so several steps retrying at once
		// queue behind each other's publishes, and during a GitHub outage the last
		// waits one bound for each. It is a cost only a step that is going again pays.
		// Retrying stays set while the next attempt runs, and its wording claims only
		// what stays true (retrySentence), because nothing publishes again when that
		// attempt begins: a publish there would delay the attempt itself by a GitHub
		// round trip. It ends when that attempt's record arrives (setAttempts) or with
		// the receipt (settle). A takeover does not restate it before the step
		// settles; the row's history, which the OS reads, is not affected. Covered by
		// TestTheCheckRunSaysAStepIsRetryingWhileItWaits.
		t.setRetrying(max(nextAt.Sub(dr.d.now()), 0))
		dr.progress(ctx)
		// Its resources, before the next attempt can claim the same slot or
		// machine. Unconfirmed, nothing more is started: confirmReceiptCleanup
		// leaves the run for recovery, whose replica finds this outcome on the
		// row and continues from it once the cleanup is confirmed.
		if !dr.confirmReceiptCleanup(ctx, exec, req) || dr.cleanupFailed.Load() {
			return
		}
		if !dr.beginNextAttempt(ctx, t, next, nextAt, ceilingAt, dr.endingReceipt(step, policy, history, rec)) {
			return
		}
	}
}

// continueAttempts carries on a step a previous driver left between two
// attempts (resume): the attempt t names ended, its record is the history's
// last, and its resources were confirmed gone before this drive started
// anything (openWork). The next attempt goes where chooseNext places it -- from
// the same history and the same captured policy the driver that recorded the
// outcome decided from, so at the place that driver chose -- once the row's
// nextAttemptAt has come, under every stop the loop has between two attempts
// (beginNextAttempt). A step chooseNext gives no next attempt, or whose next
// attempt could not start with a second of the run's ceiling left (this
// replica's ceiling may be shorter), ends now with the ended attempt's failure
// (or, after a search that only found the fleet full, the one before it:
// endingReceipt), rebuilt from its record. It answers false when the step's
// goroutine is done.
func (dr *runDriver) continueAttempts(ctx context.Context, t *stepTrack, policy pipelines.ComputePlacementPolicy, ceilingAt time.Time) bool {
	history := t.snapshot().Attempts
	nextAt := t.nextAttemptAt
	t.betweenAttempts, t.nextAttemptAt = false, time.Time{}
	last := dr.endingReceipt(t.step, policy, history, dr.recordReceipt(history[len(history)-1]))
	next, _, ok := chooseNext(t.step, policy, history)
	switch {
	case !ok:
		dr.log.Warn("pipelines: a resumed step has no next attempt by its retry rules; it ends with the attempt its row records",
			"step", t.step.Key, "attempt", t.attempt)
		dr.settle(ctx, t, last)
		return false
	case ceilingAt.Sub(nextAt) < time.Second:
		dr.log.Info("pipelines: a step's retries stop at its run's ceiling", "step", t.step.Key, "attempt", t.attempt, "nextAttemptAt", nextAt)
		dr.settle(ctx, t, last)
		return false
	}
	dr.log.Info("pipelines: a step resumed between two attempts continues at the next",
		"step", t.step.Key, "attempt", t.attempt+1, "next", describePlacement(next), "nextAttemptAt", nextAt)
	return dr.beginNextAttempt(ctx, t, next, nextAt, ceilingAt, last)
}

// beginNextAttempt waits until the step's next attempt may start and then
// writes its intent: a new attempt, never a recovery, placed where chooseNext
// said. last is the receipt that ends the step when the next cannot start: the
// attempt that just ended, or the failure before it when that attempt only found
// the fleet full (endingReceipt). It answers false when the step's goroutine is
// done, having written:
//
//   - nothing, when the workflow call that runs the step ended during the wait
//     (ctx): the execution is stopping around the step, and a write on an ended
//     call would only fail and read as a journal outage; the row still says when
//     the next attempt is due, for whichever drive carries the run on;
//   - nothing, when the lease was lost: the replica holding the run now starts
//     the next attempt from the row;
//   - a cancelled receipt, on a cancel that came as the attempt ended or during
//     the wait (the wait returns at once on one): the step had an attempt to
//     come, so no failure ended it -- the cancel did;
//   - nothing, when another step's cleanup failed during the wait: the drive
//     admits no new work and stops for recovery, which starts this step's next
//     attempt from the row;
//   - last, when the wait outlasted the run (a process that slept through it):
//     no attempt starts with less than a second of the ceiling left, and the
//     step ends with the failure whose resources were acknowledged before the
//     wait.
//
// The intent's write is the ownership check: the journal's write guard reads
// the lease fresh under the run's gate, and a write it cannot make stops the
// drive for recovery rather than leave the step running here.
func (dr *runDriver) beginNextAttempt(ctx context.Context, t *stepTrack, next *pipelines.AttemptPlacement, nextAt, ceilingAt time.Time, last receipt) bool {
	dr.awaitNextAttempt(ctx, nextAt, ceilingAt)
	switch {
	case ctx.Err() != nil:
		return false
	case dr.lease.isLost():
		return false
	case dr.lease.isCancelled():
		dr.settle(ctx, t, cancelReceipt())
		return false
	case dr.cleanupFailed.Load():
		return false
	case ceilingAt.Sub(dr.d.now()) < time.Second:
		dr.log.Info("pipelines: a step's retries stop at its run's ceiling", "step", t.step.Key, "attempt", t.attempt)
		dr.settle(ctx, t, last)
		return false
	}
	t.attempt++
	t.sent, t.placement = false, next
	return dr.recordIntent(ctx, t)
}

// recordIntent writes the intent of the attempt t is about to send: its number,
// and the records of every attempt before it. The binding names no
// nextAttemptAt, which replaces a Retrying's (the binding is replaced whole). A
// first attempt has no history, so its intent is exactly what Step has always
// written. A failed write stops the drive: an attempt that cannot be recorded
// must not start.
func (dr *runDriver) recordIntent(ctx context.Context, t *stepTrack) bool {
	handle, err := dr.work().StepAttempt(ctx, t.step.Key, t.attempt, attemptsBinding(nil, t.snapshot().Attempts, time.Time{}))
	if err != nil {
		dr.journalUnavailable(t.step.Key, err)
		return false
	}
	t.handle = handle
	return true
}

// attemptReceipt is the receipt one attempt would end its step with.
// requiresVerifiedArtifacts is whether the step declares artifacts (receiptFor).
func (dr *runDriver) attemptReceipt(end stepEnd, ceiling time.Duration, requiresVerifiedArtifacts bool) receipt {
	switch end.kind {
	case endCancelled:
		return cancelReceipt()
	case endTimeout:
		return failReceipt(pipelines.CodeRunCeiling, fmt.Sprintf(
			"The step did not report before its run's %s ceiling and the %s the runner is given past it, so the driver stopped waiting for it.",
			ceiling, stepGrace))
	case endError:
		return failReceipt(pipelines.CodeExecutorError,
			"The runner could not report how the step ended: "+dr.mask(end.err.Error()))
	}
	return dr.receiptFor(end.result, end.elapsed, requiresVerifiedArtifacts)
}

// endingReceipt is the receipt a step that ends now settles with: rec, its last
// attempt's, unless that attempt only found the fleet exhausted, when it is the
// failure before it, rebuilt from its record (failureToSettle, recordReceipt).
// Every path that ends a step after an attempt asks it: no next attempt, and
// the run's ceiling before the next one, here or on a replica that took the
// run over (continueAttempts).
func (dr *runDriver) endingReceipt(step pipelines.Step, policy pipelines.ComputePlacementPolicy, history []attemptRecord, rec receipt) receipt {
	if r, ok := failureToSettle(step, policy, history); ok {
		return dr.recordReceipt(r)
	}
	return rec
}

// nextAttempt is where the step's next attempt goes and when it may start, or
// false when the step ends with this attempt. Only a runner's answer that
// failed is retried, and only as the executor classified it (chooseNext); a
// timeout, an error and a cancel are final. A next attempt that could not start
// with a second of the run's ceiling left is no next attempt: the wait is not
// begun, so the step ends now with its failure (endingReceipt) rather than at
// the ceiling with nothing more to show.
func (dr *runDriver) nextAttempt(step pipelines.Step, policy pipelines.ComputePlacementPolicy, end stepEnd, rec receipt, history []attemptRecord, ceilingAt time.Time) (*pipelines.AttemptPlacement, time.Time, bool) {
	if end.kind != endResult || rec.status != WorkStepFailed {
		return nil, time.Time{}, false
	}
	next, delay, ok := chooseNext(step, policy, history)
	if !ok {
		return nil, time.Time{}, false
	}
	nextAt := dr.d.now().Add(delay)
	if ceilingAt.Sub(nextAt) < time.Second {
		dr.log.Info("pipelines: a step's retries stop at its run's ceiling", "step", step.Key, "attempt", history[len(history)-1].Attempt, "nextAttemptAt", nextAt)
		return nil, time.Time{}, false
	}
	return next, nextAt, true
}

// awaitNextAttempt waits until the next attempt may start, never into the last
// second of the run's ceiling, and returns early when the drive stops -- the
// lease's stop channel closes on a cancel and on a lost lease alike -- or when
// the workflow call that runs the step ends (ctx). The caller reads which, and
// the ceiling, when it returns. Deps.retryTimer stands in for the timer in a
// test.
func (dr *runDriver) awaitNextAttempt(ctx context.Context, nextAt, ceilingAt time.Time) {
	now := dr.d.now()
	wait := min(nextAt.Sub(now), ceilingAt.Add(-time.Second).Sub(now))
	if wait <= 0 {
		return
	}
	var fired <-chan time.Time
	if dr.d.retryTimer != nil {
		fired = dr.d.retryTimer(wait)
	} else {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		fired = timer.C
	}
	select {
	case <-fired:
	case <-dr.lease.stop:
	case <-ctx.Done():
	}
}

// retryPolicy is the run's captured compute placement policy, whose retry
// limits every command step is held to, or the zero policy -- no retries --
// for a drive that captured none.
func (dr *runDriver) retryPolicy() pipelines.ComputePlacementPolicy {
	if dr.computePlacement == nil {
		return pipelines.ComputePlacementPolicy{}
	}
	return dr.computePlacement.Clone()
}

func (dr *runDriver) acknowledgeReceipt(ctx context.Context, exec pipelines.Executor, req pipelines.StepRequest) bool {
	return dr.confirmReceiptCleanup(ctx, exec, req)
}

// Recovery can confirm independent recorded receipts before relinquishing its
// lease. Live execution drains active commands first, admitting no new work.
func (dr *runDriver) confirmReceiptCleanup(ctx context.Context, exec pipelines.Executor, req pipelines.StepRequest) bool {
	if !dr.stillHolds(ctx) {
		// A transient ownership-read failure must not let runStep continue to
		// a successful verdict without having attempted cleanup.
		if !dr.lease.isLost() {
			dr.cleanupFailed.Store(true)
			dr.lease.lose()
		}
		return false
	}
	var err error
	if exec == nil {
		err = errors.New("no executor is available to confirm cleanup")
	}
	if acknowledger, ok := exec.(pipelines.ReceiptAcknowledger); ok {
		// Cleanup needs identity, not credentials. No resolved secret survives
		// into an acknowledgement or a recovered driver's cleanup.
		req.Secrets = nil
		cleanupCtx, cancel := context.WithTimeout(ctx, executorCancelTimeout)
		err = acknowledger.AcknowledgeReceipt(cleanupCtx, req)
		cancel()
	}
	if err != nil {
		dr.log.Warn("pipelines: resource cleanup remains unconfirmed; keeping the run unfinished for recovery", "step", req.StepKey, "error", dr.mask(err.Error()))
		dr.cleanupFailed.Store(true)
		return false
	}
	return true
}

// settle writes a step's receipt -- its intent first, for a step this drive
// has not begun, so every declared step reads as begun and ended -- and
// records it in memory, the artifacts it names included: a notify stage later
// in the run lists them. On a lost lease it writes nothing.
//
// A step the executor was handed has an attempt history (StepState.Attempts),
// and its receipt restates all of it beside where the last attempt ran,
// whatever ended the step -- the last attempt's answer, a cancel between
// attempts, the cancel sweep after the workflow, a resumed run ended without
// running it. The binding is replaced whole on every write that names it, so a
// receipt without the history would erase it, and one that named no binding
// would keep the nextAttemptAt of the wait it ended (attempts.go).
func (dr *runDriver) settle(ctx context.Context, t *stepTrack, rec receipt) bool {
	if !dr.stillHolds(ctx) {
		return false
	}
	if t.handle == nil {
		// The intent names the attempt the step is at and restates its history.
		// For a step that never ran that is attempt 1 and none, exactly what Step
		// writes. For one a previous driver took to attempt 3, the row must not
		// read as attempt 1 from here on, and an intent with no binding would
		// keep the stored one, nextAttemptAt included, until the receipt.
		var err error
		t.handle, err = dr.work().StepAttempt(ctx, t.step.Key, t.attempt, attemptsBinding(nil, t.snapshot().Attempts, time.Time{}))
		if err != nil {
			dr.journalUnavailable(t.step.Key, err)
			return false
		}
	}
	if attempts := t.snapshot().Attempts; len(attempts) > 0 {
		rec.binding = attemptsBinding(rec.binding, attempts, time.Time{})
	}
	if rec.result == nil {
		rec.result = map[string]any{}
	}
	if _, recorded := rec.result["summary"]; !recorded {
		rec.result["summary"] = strings.ReplaceAll(t.step.Stage+" / "+t.step.Name, "-", " ") + ": " + rec.report
	}
	if err := t.handle.Finish(ctx, rec.journal()); err != nil {
		dr.journalUnavailable(t.step.Key, err)
		return false
	}
	s := t.snapshot()
	s.Status, s.Code, s.Message, s.DurationMs, s.LogTail = rec.report, rec.code, rec.message, rec.durationMs, rec.logTail
	s.ArtifactFileIDs = slices.Clone(rec.artifactFileIDs)
	// The receipt ends any retry: whatever the step settled as, it is not going again.
	s.Retrying, s.RetryAfter = false, 0
	t.set(s)
	return true
}

// A missing intent or receipt is not permission to continue. Leave the run
// unfinished for recovery, stop renewing this drive, and publish no verdict.
func (dr *runDriver) journalUnavailable(step string, err error) {
	if driveStopped(err) {
		return
	}
	dr.d.Logger.Error("pipelines: journal write failed; stopping this drive for recovery", "runId", dr.runID, "step", step, "error", dr.mask(err.Error()))
	dr.journalFailed.Store(true)
	dr.lease.lose()
}

// endKind is how waiting on a step ended.
type endKind int

const (
	endResult endKind = iota
	endError
	endTimeout
	endCancelled
	endLost
)

type stepEnd struct {
	kind    endKind
	result  pipelines.StepResult
	err     error
	elapsed time.Duration
}

// execStep hands one step to the executor and waits for its answer, for the
// hard deadline, or for the drive to stop -- whichever comes first. The
// executor runs in a goroutine of its own, so a runner that never answers
// cannot hold the drive: the deadline is HARD.
func (dr *runDriver) execStep(exec pipelines.Executor, req pipelines.StepRequest, deadline time.Duration) stepEnd {
	stepCtx, cancel := context.WithTimeout(dr.execCtx, deadline)
	type answer struct {
		res pipelines.StepResult
		err error
	}
	answered := make(chan answer, 1)
	began := time.Now()
	go func() {
		// The step's context is released when the runner answers, whoever
		// stopped waiting first -- and not before on a lost lease, where the
		// replica taking over re-attaches to this very work.
		defer cancel()
		res, err := exec.Execute(stepCtx, req)
		answered <- answer{res: res, err: err}
	}()

	var (
		a   answer
		got bool
	)
	select {
	case a = <-answered:
		got = true
	case <-stepCtx.Done():
	case <-dr.lease.stop:
	}
	if !got {
		// The runner may have answered in the same instant: the goroutine
		// above releases the step's context right after it delivers the
		// answer, so both can be ready by the time this select runs, and a
		// select picks among ready cases at random. An answer that arrived
		// is the answer -- a passed step must never be read as cancelled.
		select {
		case a = <-answered:
			got = true
		default:
		}
	}
	elapsed := time.Since(began)
	switch {
	case dr.lease.isLost():
		return stepEnd{kind: endLost}
	case dr.lease.isCancelled():
		return stepEnd{kind: endCancelled}
	case got && a.err == nil:
		return stepEnd{kind: endResult, result: a.res, elapsed: elapsed}
	case errors.Is(stepCtx.Err(), context.DeadlineExceeded):
		return stepEnd{kind: endTimeout, elapsed: elapsed}
	case got:
		return stepEnd{kind: endError, err: a.err, elapsed: elapsed}
	}
	// The step's context ended with neither a deadline nor a cancel this
	// drive asked for: the drive is ending around it.
	return stepEnd{kind: endCancelled}
}

// stepDeadline is how long the driver waits on a step it hands the runner:
// until its run's ceiling and the grace the runner is given past it (ruling
// R31b, design record D11) -- not the step's own timeout. A step may wait in
// the cluster's queue for a free slot under the pipelines ceiling, bounded by
// its run's ceiling alone, and its own timeout runs from its Job's creation;
// the runner holds it to both. Measured on the driver's clock from the run's
// start that every request carries, so the driver and the executor bound a
// step by the same moment.
func (dr *runDriver) stepDeadline(ceiling time.Duration) time.Duration {
	return dr.runCeilingAt(ceiling).Add(stepGrace).Sub(dr.d.now())
}

// runCeilingAt is the moment the run reaches its ceiling: its start, as every
// request carries it, plus the ceiling. The executor refuses to start a step
// past it, so the driver neither waits into it for a retry nor starts one in
// its last second (runStep).
func (dr *runDriver) runCeilingAt(ceiling time.Duration) time.Time {
	started, err := time.Parse(time.RFC3339, dr.facts.startedAt)
	if err != nil {
		started = dr.d.now()
	}
	return started.Add(ceiling)
}

// requestFacts are the run's facts every StepRequest carries, fixed once the
// work run is open, so a step's goroutine never reads the drive's row.
type requestFacts struct {
	runID, workRunID, pipelineID, owner string
	attempt                             int
	startedAt                           string
	repository                          pipelines.Repository
	sha, baseSHA, version               string
	mode                                pipelines.Mode
	event                               pipelines.Event
	installation                        int64
	compute                             pipelines.Compute
	domain                              string

	// What a notification says of the run besides (notify.go): its commit's
	// or pull request's title, the branch, the pull request, and when the run
	// was queued and started.
	title, branch     string
	pullRequest       int
	queuedAt, started time.Time
}

func (dr *runDriver) setFacts() {
	owner, name, _ := splitRepository(dr.p.Repository)
	version := dr.run.Version
	if version == "" {
		version = dr.run.SHA
	}
	started := dr.run.StartedAt
	if started.IsZero() {
		started = dr.d.now()
	}
	dr.facts = requestFacts{
		runID:      bareID(dr.run.ID),
		workRunID:  bareID(dr.work().RunID()),
		pipelineID: bareID(dr.p.ID),
		owner:      dr.p.OwnerUserID,
		attempt:    max(dr.run.Attempt, 1),
		startedAt:  started.UTC().Format(time.RFC3339),
		repository: pipelines.Repository{
			Owner: owner, Name: name, CloneURL: "https://github.com/" + owner + "/" + name + ".git",
		},
		sha: dr.run.SHA, baseSHA: dr.run.BaseSHA, version: version, mode: dr.run.Mode, event: dr.run.Event,
		installation: dr.installation, compute: dr.p.Compute,
		domain: dr.d.Domain(),
		title:  dr.run.Title, branch: dr.run.HeadBranch, pullRequest: dr.run.PullRequest,
		queuedAt: dr.run.QueuedAt, started: started,
	}
}

// request is what the executor is handed for one step. Ids are bare; the
// secrets are this step's resolved values and exist nowhere else.
func (dr *runDriver) request(t *stepTrack, secrets map[string]string) pipelines.StepRequest {
	f := dr.facts
	// A step receives its own value: an executor cannot mutate the captured
	// placement decision observed by a sibling step or a resumed driver (Clone
	// copies the retry delays too; they are a slice). Every command step is handed
	// the policy, not only a pool step, because its retry limits apply to all of
	// them; the executor reads the capacity half for a pool step alone
	// (TestAPolicyOnADedicatedStepChangesNoRoute). Recovery binds the policy for
	// the same steps (definitionForRun), so what is sent is what is bound.
	var placement *pipelines.ComputePlacementPolicy
	if dr.computePlacement != nil && t.step.Kind == pipelines.StepCommand {
		copy := dr.computePlacement.Clone()
		placement = &copy
	}
	// The attempt's own place, chooseNext's for a retry and none for a first
	// attempt, copied for the reason the policy is: what the executor is handed
	// is its own.
	var attemptPlacement *pipelines.AttemptPlacement
	if t.placement != nil {
		copy := *t.placement
		copy.ExcludeWorkers = slices.Clone(t.placement.ExcludeWorkers)
		attemptPlacement = &copy
	}
	return pipelines.StepRequest{
		ComputePlacement: placement,
		Placement:        attemptPlacement,
		RecoverOnly:      t.sent,
		RunID:            f.runID,
		WorkRunID:        f.workRunID,
		StepKey:          t.step.Key,
		Attempt:          t.attempt,
		RunAttempt:       f.attempt,
		RunStartedAt:     f.startedAt,
		PipelineID:       f.pipelineID,
		OwnerUserID:      f.owner,
		Repository:       f.repository,
		SHA:              f.sha,
		BaseSHA:          f.baseSHA,
		Mode:             f.mode,
		Event:            f.event,
		HeadBranch:       f.branch,
		Version:          f.version,
		Domain:           f.domain,
		InstallationID:   f.installation,
		Compute:          f.compute,
		Step:             t.step,
		Secrets:          secrets,
	}
}

// ---------------------------------------------------------------------------
// Receipts
// ---------------------------------------------------------------------------

// receipt is one step's ending, in both vocabularies: the work step's
// (status) and the check run's (report).
type receipt struct {
	status, report  string
	code, message   string
	durationMs      int64
	binding         map[string]any
	logFileID       string
	artifactFileIDs []string
	result          map[string]any
	// logTail stays in memory, UNMASKED, until the check run masks it; it is
	// never written to a row.
	logTail string
}

func (r receipt) journal() workjournal.Receipt {
	return workjournal.Receipt{
		Status: r.status, Result: r.result, Code: r.code, Message: r.message, DurationMs: r.durationMs,
		Binding: r.binding, LogFileID: r.logFileID, ArtifactFileIDs: r.artifactFileIDs,
	}
}

func skipReceipt(code, why string) receipt {
	return receipt{status: WorkStepSkipped, report: StepSkipped, code: code, message: why,
		result: map[string]any{"reason": why, "code": code}}
}

func failReceipt(code, message string) receipt {
	return receipt{status: WorkStepFailed, report: StepFailed, code: code, message: message}
}

func cancelReceipt() receipt {
	const why = "The run was cancelled before this step finished."
	return receipt{status: WorkStepCancelled, report: StepCancelled, message: why,
		result: map[string]any{"reason": why}}
}

// receiptFor reads the executor's answer into a receipt: the outcome, the
// failure, where it ran, how long it took, its log and artifacts, and the
// metadata a person reads beside it. EVERY string the runner wrote is masked
// before it is kept -- a code and a status as much as a message, a job name
// and a machine label as much as a log line: none of them is the driver's to
// vouch for, and each reaches a row, the check run or a log line.
func (dr *runDriver) receiptFor(res pipelines.StepResult, elapsed time.Duration, requiresVerifiedArtifacts bool) receipt {
	masks := dr.maskValues()
	mask := func(s string) string { return pipelines.MaskSecrets(strings.TrimSpace(s), masks) }
	rec := receipt{
		durationMs:      durationOf(res, elapsed),
		binding:         bindingOf(res.Where, mask),
		logFileID:       mask(res.LogFileID),
		artifactFileIDs: maskAll(res.ArtifactFileIDs, mask),
		logTail:         res.LogTail, // masked when the check run quotes it (report)
	}
	status := mask(string(res.Status))
	switch res.Status {
	case pipelines.OutcomeSucceeded:
		rec.status, rec.report = WorkStepDone, StepSucceeded
	case pipelines.OutcomeFailed:
		rec.status, rec.report = WorkStepFailed, StepFailed
	case pipelines.OutcomeRefused:
		rec.status, rec.report = WorkStepFailed, StepRefused
	case pipelines.OutcomeCancelled:
		rec.status, rec.report = WorkStepCancelled, StepCancelled
	default:
		rec.status, rec.report = WorkStepFailed, StepFailed
		rec.code = pipelines.CodeExecutorError
		rec.message = fmt.Sprintf("The runner answered an outcome this driver does not know (%q).", status)
	}
	if res.Failure != nil {
		rec.code, rec.message = mask(res.Failure.Code), mask(res.Failure.Message)
	}
	if rec.message == "" && rec.status == WorkStepFailed {
		rec.message = exitMessage(res.ExitCode)
	}
	notes := make([]map[string]any, 0, len(res.Notes))
	for _, n := range res.Notes {
		notes = append(notes, map[string]any{"code": mask(n.Code), "message": mask(n.Message)})
	}
	metadata := map[string]any{
		"exitCode":  res.ExitCode,
		"logLines":  res.LogLines,
		"logCapped": res.LogCapped,
	}
	if len(notes) > 0 {
		metadata["notes"] = notes
	}
	validIntents := pipelines.ValidArtifactIntentIDs(res.ArtifactIntentIDs)
	for _, id := range res.ArtifactIntentIDs {
		validIntents = validIntents && mask(id) == id
	}
	if !validIntents || (requiresVerifiedArtifacts && res.Status == pipelines.OutcomeSucceeded && len(res.ArtifactIntentIDs) == 0) {
		rec.status, rec.report = WorkStepFailed, StepFailed
		rec.code, rec.message = pipelines.CodeArtifactUnavailable, "The step returned no complete, valid artifact receipt identities."
		status = string(pipelines.OutcomeFailed)
	} else if len(res.ArtifactIntentIDs) > 0 {
		metadata["artifactIntentIds"] = slices.Clone(res.ArtifactIntentIDs)
	}
	rec.result = map[string]any{"status": status, "metadata": metadata}
	// A package path is written to the pipeline's timing table: one that
	// would need masking is no package path, and is not kept.
	timings := make(map[string]float64, len(res.Timings))
	for path, seconds := range res.Timings {
		if mask(path) == strings.TrimSpace(path) {
			timings[path] = seconds
		}
	}
	dr.observe(timings)
	return rec
}

// maskAll is each of values through mask, blanks dropped.
func maskAll(values []string, mask func(string) string) []string {
	var out []string
	for _, v := range values {
		if m := mask(v); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// durationOf is the step's time as the runner measured it, from its own
// start and finish, or else how long the driver waited.
func durationOf(res pipelines.StepResult, waited time.Duration) int64 {
	started, err1 := time.Parse(time.RFC3339, strings.TrimSpace(res.StartedAt))
	finished, err2 := time.Parse(time.RFC3339, strings.TrimSpace(res.FinishedAt))
	if err1 == nil && err2 == nil && !finished.Before(started) {
		if ms := finished.Sub(started).Milliseconds(); ms > 0 {
			return ms
		}
	}
	return max(waited.Milliseconds(), 1)
}

// bindingOf is where a step ran, as the step's binding records it, every
// value through mask; an empty field is left out rather than written blank.
func bindingOf(w pipelines.Where, mask func(string) string) map[string]any {
	out := map[string]any{}
	for name, value := range map[string]string{
		"surface": w.Surface, "nodeId": w.NodeID, "jobName": w.JobName, "workerId": w.WorkerID,
	} {
		if v := mask(value); v != "" {
			out[name] = v
		}
	}
	if len(w.MachineLabels) > 0 {
		labels := make(map[string]string, len(w.MachineLabels))
		for k, v := range w.MachineLabels {
			labels[mask(k)] = mask(v)
		}
		out["machineLabels"] = labels
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// exitMessage is a failed step's sentence when the runner gave none.
func exitMessage(code int) string {
	if code < 0 {
		return "The step's command did not run."
	}
	return fmt.Sprintf("The step's command exited with status %d.", code)
}

// ---------------------------------------------------------------------------
// The verdict and the conclusion
// ---------------------------------------------------------------------------

// verdictOfSteps is how the steps ended: cancelled when the run was and the
// cancel cut something short, failed when any step did not pass, success
// otherwise. A cancel that arrives after every step has ended has nothing to
// stop, and the check run reports what the steps did.
func (dr *runDriver) verdictOfSteps() verdict {
	if dr.lease.isCancelled() {
		for _, t := range dr.tracks {
			if s := t.snapshot(); s.Status == StepCancelled || !t.finished() {
				return verdict{conclusion: ConclusionCancelled, workMessage: "The run was cancelled."}
			}
		}
	}
	for _, t := range dr.tracks {
		s := t.snapshot()
		switch s.Status {
		case StepFailed, StepRefused, StepCancelled:
			return verdict{conclusion: ConclusionFailure, workCode: s.Code, workMessage: "Failed at " + s.Stage + ": " + s.Key}
		case StepSkipped:
			if s.Code == pipelines.CodeStageBlocked {
				// DSL can block work, but an unexecuted required step is not
				// evidence of success, even if no other step recorded failure.
				return failedOutsideSteps(s.Code, "Required step "+s.Key+" was blocked.")
			}
		case StepPending, StepRunning:
			// A step that never ended never passed.
			return failedOutsideSteps("", "Step "+s.Key+" never reported how it ended.")
		}
	}
	return verdict{conclusion: ConclusionSuccess}
}

// conclude ends the run: the work run closed, the check run completed, the
// run row completed -- in that order, so a replica that stops half-way leaves
// the run unfinished, to be recovered and concluded again, rather than a work
// run open that no sweep closes (a pipeline's run is its runner's own). Only
// individual run and journal writes are under the gate, each with a fresh
// ownership check. GitHub calls remain outside it and require destination
// reconciliation if ownership changes while a request is in flight. A
// successful FULL run then teaches the pipeline its packages' timings. It answers whether the run is concluded.
func (dr *runDriver) conclude(ctx context.Context, v verdict) bool {
	if !dr.stillHolds(ctx) {
		return false
	}
	if dr.work() == nil && strings.TrimSpace(dr.run.WorkRunID) != "" {
		why := "The run was cancelled before this step finished."
		if v.refusal != nil {
			why = "The run ended before this step finished: " + v.refusal.Detail
		}
		if !dr.settleStoredWork(ctx, receipt{status: WorkStepCancelled, report: StepCancelled, message: why,
			result: map[string]any{"reason": why}}) {
			return false
		}
	}
	// What this conclusion leaves in flight is stopped: everything, on a
	// cancel; the steps a predecessor handed the runner and this drive
	// abandoned, on any other ending. Never on a lost lease -- that work is
	// the new driver's to re-attach to.
	if dr.lease.isLost() {
		return false
	}
	if v.conclusion == ConclusionCancelled || dr.abandonedInFlight {
		if !dr.cancelRunner() {
			return false
		}
	}

	if err := dr.releaseRepositoryChangeEvidence(ctx); err != nil {
		dr.log.Warn("pipelines: completed repository change evidence awaits cleanup", "error", dr.mask(err.Error()))
		return false
	}
	now := dr.d.now()
	stages := dr.stageSummaries()
	final := dr.run
	final.Status, final.Conclusion, final.FinishedAt, final.Stages = StatusCompleted, v.conclusion, now, stages
	patch := RunPatch{
		Status: ptr(StatusCompleted), Conclusion: ptr(v.conclusion), FinishedAt: ptr(now), Stages: ptr(stages),
	}
	if !final.StartedAt.IsZero() {
		final.DurationMs = max(now.Sub(final.StartedAt).Milliseconds(), 0)
		patch.DurationMs = ptr(final.DurationMs)
	}
	if v.refusal != nil {
		final.RefusalCode, final.RefusalMessage, final.RefusalScope = v.refusal.Code, v.refusal.Detail, v.refusal.Scope
		patch.RefusalCode, patch.RefusalMessage, patch.RefusalScope = ptr(v.refusal.Code), ptr(v.refusal.Detail), ptr(v.refusal.Scope)
	}
	if v.outsideSteps && v.conclusion == ConclusionFailure && strings.TrimSpace(v.workMessage) != "" {
		// The run's own reason goes on the ROW, in this same write as the
		// conclusion: the final check run is composed from `final`, and a
		// republish (recover.go) or another replica has only the row. Masked
		// here, whoever built the verdict, as every reported text is.
		message := dr.mask(v.workMessage)
		final.FailureCode, final.FailureMessage = v.workCode, message
		patch.FailureCode, patch.FailureMessage = ptr(v.workCode), ptr(message)
	}

	// The fence again, before the writes that take no gate: settling the
	// stored work above may be what found the lease gone.
	if !dr.stillHolds(ctx) {
		return false
	}
	var journalErr error
	switch v.conclusion {
	case ConclusionSuccess:
		journalErr = dr.work().Succeeded(ctx, map[string]any{"conclusion": v.conclusion, "stages": stageList(stages)})
	case ConclusionCancelled:
		journalErr = dr.work().Cancelled(ctx, v.workCode, v.workMessage)
	default:
		journalErr = dr.work().Failed(ctx, v.workCode, v.workMessage)
	}
	if journalErr != nil {
		dr.journalUnavailable("", journalErr)
		return false
	}
	var written *CheckRunWrite
	if dr.havePipeline {
		w := dr.publishFinal(ctx, final)
		written = &w
	}

	err := dr.underLease(ctx, func(gctx context.Context, current Run) error {
		if written != nil {
			// A final report that did not land, after its retries, is
			// recorded `unavailable` -- a check run left showing the run
			// unfinished is not "written" -- and recovery republishes it.
			if cr, changed := finalCheckRunPatch(*written, current); changed {
				patch.CheckRunID, patch.CheckRunState, patch.Notes = cr.CheckRunID, cr.CheckRunState, cr.Notes
			}
			if id := dr.run.CheckRunID; id > 0 && id != current.CheckRunID && patch.CheckRunID == nil {
				// A check run this drive created whose id never reached the
				// row: the row learns it now, or the next re-run's reader
				// finds no check run to name.
				patch.CheckRunID = ptr(id)
			}
		}
		return dr.d.Store.UpdateRun(gctx, current.OwnerUserID, current.ID, patch)
	})
	if err != nil {
		dr.abort("concluding the run", err)
		return false
	}
	applyRunPatch(&dr.run, patch)
	dr.log.Info("pipelines: the run concluded", "conclusion", v.conclusion, "code", v.workCode)
	if err := dr.i.reconcileMaintenance(ctx, dr.d, dr.runID); err != nil {
		dr.log.Warn("pipelines: failed-run diagnosis awaits recovery", "error", err)
	}
	if v.conclusion == ConclusionSuccess && dr.run.Mode == pipelines.ModeFull && dr.havePipeline {
		if err := dr.i.mergeTimings(ctx, dr.d, dr.p, dr.runID, dr.observedTimings()); err != nil {
			dr.log.Warn("pipelines: the run's package timings were not merged into the pipeline's table", "error", err)
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

// writeRun writes patch to the run row under the lease, and the drive's copy
// of the row follows it.
func (dr *runDriver) writeRun(ctx context.Context, patch RunPatch) error {
	return dr.underLease(ctx, func(gctx context.Context, current Run) error {
		if err := dr.d.Store.UpdateRun(gctx, current.OwnerUserID, current.ID, patch); err != nil {
			return err
		}
		applyRunPatch(&dr.run, patch)
		return nil
	})
}

// progress records the stages as they stand and moves the check run, one caller
// at a time (progressMu).
func (dr *runDriver) progress(ctx context.Context) {
	dr.progressMu.Lock()
	defer dr.progressMu.Unlock()
	if err := dr.writeRun(ctx, RunPatch{Stages: ptr(dr.stageSummaries())}); err != nil {
		if !driveStopped(err) {
			dr.log.Warn("pipelines: the run's stage table was not recorded", "error", err)
		}
		return
	}
	dr.publish(ctx)
}

// stageSummaries is the run row's stage table, in plan order.
func (dr *runDriver) stageSummaries() []StageSummary {
	out := make([]StageSummary, 0, len(dr.stages))
	for _, st := range dr.stages {
		states := make([]StepState, 0, len(st.tracks))
		for _, t := range st.tracks {
			states = append(states, t.snapshot())
		}
		failed := 0
		for _, s := range states {
			if s.Status == StepFailed || s.Status == StepRefused {
				failed++
			}
		}
		out = append(out, StageSummary{
			Name: st.name, Status: stageStatus(states), DurationMs: stageDuration(states),
			Steps: len(states), Failed: failed,
		})
	}
	return out
}

// stageStatus settles a stage's steps into one status, as the check run's
// table settles them (pipelines.CheckOutput): running while any step is, or
// once some finished and others wait; failed, cancelled, waiting, blocked,
// skipped; passed when a step passed and nothing worse happened.
func stageStatus(states []StepState) string {
	var passed, failed, cancelled, skipped, blocked, running, waiting int
	for _, s := range states {
		switch s.Status {
		case StepSucceeded:
			passed++
		case StepFailed, StepRefused:
			failed++
		case StepCancelled:
			cancelled++
		case StepRunning:
			running++
		case StepSkipped:
			if s.Code == pipelines.CodeStageBlocked {
				blocked++
			} else {
				skipped++
			}
		default:
			waiting++
		}
	}
	switch {
	case running > 0 || (waiting > 0 && passed+failed+cancelled > 0):
		return StageRunning
	case failed > 0:
		return StageFailed
	case cancelled > 0:
		return StageCancelled
	case waiting > 0:
		return StageWaiting
	case blocked > 0 && passed == 0:
		return StageBlocked
	case passed == 0:
		return StageSkipped
	}
	return StagePassed
}

// stageDuration is a stage's time: its longest step's, since its steps run at
// once.
func stageDuration(states []StepState) int64 {
	var longest int64
	for _, s := range states {
		switch s.Status {
		case StepSucceeded, StepFailed, StepRefused, StepCancelled:
			longest = max(longest, s.DurationMs)
		}
	}
	return longest
}

// applyRunPatch moves an in-memory row by a patch the store accepted.
func applyRunPatch(r *Run, p RunPatch) {
	if p.SelectionFacts != nil {
		r.SelectionFacts = maps.Clone(*p.SelectionFacts)
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&r.MaintenanceState, p.MaintenanceState)
	set(&r.MaintenanceGoalID, p.MaintenanceGoalID)
	set(&r.MaintenanceMessage, p.MaintenanceMessage)
	set(&r.Status, p.Status)
	set(&r.Conclusion, p.Conclusion)
	set(&r.RefusalCode, p.RefusalCode)
	set(&r.RefusalMessage, p.RefusalMessage)
	set(&r.RefusalScope, p.RefusalScope)
	set(&r.FailureCode, p.FailureCode)
	set(&r.FailureMessage, p.FailureMessage)
	set(&r.CheckRunState, p.CheckRunState)
	set(&r.WorkRunID, p.WorkRunID)
	set(&r.WorkGoalID, p.WorkGoalID)
	set(&r.DriverNodeID, p.DriverNodeID)
	set(&r.DriverLeaseID, p.DriverLeaseID)
	set(&r.CancelledBy, p.CancelledBy)
	if p.CheckRunID != nil {
		r.CheckRunID = *p.CheckRunID
	}
	if p.Notes != nil {
		r.Notes = slices.Clone(*p.Notes)
	}
	if p.DriverHeartbeatAt != nil {
		r.DriverHeartbeatAt = *p.DriverHeartbeatAt
	}
	if p.CancelRequested != nil {
		r.CancelRequested = *p.CancelRequested
	}
	if p.Stages != nil {
		r.Stages = slices.Clone(*p.Stages)
	}
	if p.StartedAt != nil {
		r.StartedAt = *p.StartedAt
	}
	if p.FinishedAt != nil {
		r.FinishedAt = *p.FinishedAt
	}
	if p.DurationMs != nil {
		r.DurationMs = *p.DurationMs
	}
}

// ---------------------------------------------------------------------------
// Masks and timings
// ---------------------------------------------------------------------------

// remember adds resolved secret values to what every text the runner reports
// is masked with.
func (dr *runDriver) remember(values ...string) {
	dr.maskMu.Lock()
	defer dr.maskMu.Unlock()
	dr.masks = append(dr.masks, values...)
}

func (dr *runDriver) maskValues() []string {
	dr.maskMu.Lock()
	defer dr.maskMu.Unlock()
	return slices.Clone(dr.masks)
}

// mask is s with every secret value this drive resolved replaced.
func (dr *runDriver) mask(s string) string { return pipelines.MaskSecrets(s, dr.maskValues()) }

// observe collects a step's package timings for the pipeline's table.
func (dr *runDriver) observe(timings map[string]float64) {
	if len(timings) == 0 {
		return
	}
	dr.timingsMu.Lock()
	defer dr.timingsMu.Unlock()
	if dr.observed == nil {
		dr.observed = map[string]float64{}
	}
	maps.Copy(dr.observed, timings)
}

func (dr *runDriver) observedTimings() map[string]float64 {
	dr.timingsMu.Lock()
	defer dr.timingsMu.Unlock()
	return maps.Clone(dr.observed)
}

// shortSHA is a commit as a person reads it.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
