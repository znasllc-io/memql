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
// call. So a replica that dies mid-run strands nothing: another claims the
// run once the lease is stale (recover.go), keeps every step that has a
// receipt, and re-sends the one that was running with the SAME attempt, which
// the runner re-attaches by (run, step key, attempt).
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
	// ruling R31b, stepDeadline).
	stepGrace = 10 * time.Minute
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
		i: i, d: d, lease: l, runID: runID, run: run,
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
	i     *Integration
	d     Deps
	lease *lease
	runID string
	log   *slog.Logger

	// run is the row as this drive last wrote or read it. Only the drive's
	// own goroutine touches it; a step's goroutine reads facts instead.
	run          Run
	p            Pipeline
	havePipeline bool
	token        string
	installation int64

	workRun atomic.Pointer[workjournal.Run]
	facts   requestFacts

	stages []stageTracks
	tracks []*stepTrack
	// abandonedInFlight: this drive ends steps a previous driver handed to
	// the runner without re-sending them, so the runner must be told to stop
	// them (reopenFromRows, conclude). The drive's goroutine only.
	abandonedInFlight bool

	execCtx    context.Context
	cancelExec context.CancelFunc

	maskMu sync.Mutex
	masks  []string

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
}

func refusedWith(r *pipelines.Refusal) verdict {
	return verdict{conclusion: ConclusionFailure, refusal: r, workCode: r.Code, workMessage: r.Detail}
}

// drive takes the run from claimed to concluded. The heartbeat renews the
// lease beside it -- through the conclusion too, which calls GitHub and may
// take a while; a lease left to go stale there would let another replica take
// the run over mid-conclusion -- and a cancel stops the runner's in-flight
// work at once.
func (dr *runDriver) drive(ctx context.Context) driveOutcome {
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
	if dr.lease.isLost() {
		return driveLost
	}
	dr.cancelExec()
	return outcome
}

// cancelRunner tells the executor to stop every step of the run it still has
// in flight. Safe with nothing running, and with no executor at all.
func (dr *runDriver) cancelRunner() {
	exec := pipelines.CurrentExecutor()
	if exec == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), executorCancelTimeout)
	defer cancel()
	if err := exec.Cancel(ctx, dr.runID); err != nil {
		dr.log.Warn("pipelines: the runner did not confirm the run's cancel", "error", dr.mask(err.Error()))
	}
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
	if !p.Active() && dr.run.Status == StatusQueued {
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
		// No stage applies to this run: a success with nothing to run, and
		// no work goal for nothing.
		return verdict{conclusion: ConclusionSuccess}, true
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

	dr.execute(ctx)
	if dr.lease.isLost() {
		return verdict{}, false
	}
	return dr.verdictOfSteps(), true
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
	byKey := make(map[string]WorkStep, len(prior))
	for _, row := range prior {
		if row.Key != "" {
			byKey[row.Key] = row
		}
	}
	for _, t := range tracks {
		row, ok := byKey[t.step.Key]
		if !ok || t.step.Skip != nil || t.step.Kind == pipelines.StepNotify {
			continue
		}
		passed := row.Status == WorkStepDone
		carried := row.Status == WorkStepSkipped && row.Skip != nil && row.Skip.Code == pipelines.CodePassedEarlier
		if !passed && !carried {
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
		call := map[string]any{"construct": "pipeline", "name": t.step.Name, "stage": t.step.Stage}
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

// resume lays the step rows a previous driver wrote over the plan read again:
// a step with a receipt keeps it and is not run again, and every other step is
// carried out as its ROW says the run planned it -- the slice it was given,
// and whether it was a skip. So a step that was running with no receipt is
// re-sent, with the same attempt, even when the plan read now would skip it:
// it was handed to the runner, and its outcome is the run's. It answers why
// the run cannot be resumed, or "": a step the rows hold and the plan does
// not is work the run began and could no longer account for.
func (dr *runDriver) resume(rows []WorkStep) string {
	stored := make(map[string]WorkStep, len(rows))
	for _, row := range rows {
		if row.Key != "" {
			stored[row.Key] = row
		}
	}
	planned := make(map[string]bool, len(dr.tracks))
	for _, t := range dr.tracks {
		planned[t.step.Key] = true
		row, ok := stored[t.step.Key]
		if !ok {
			continue
		}
		t.attempt = max(row.Attempt, 1)
		t.sent = row.Status == WorkStepRunning
		if len(row.Packages) > 0 {
			t.step.Packages = slices.Clone(row.Packages)
		}
		t.step.Skip = nil
		if row.Skip != nil {
			skip := *row.Skip
			t.step.Skip = &skip
		}
		if row.Finished() {
			t.set(stateFromRow(t.snapshot(), row))
		}
	}
	var missing []string
	for key := range stored {
		if !planned[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf("the plan read again at %s no longer holds %s", shortSHA(dr.run.SHA), strings.Join(missing, ", "))
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
	for _, t := range dr.tracks {
		if !t.finished() {
			dr.settle(ctx, t, failReceipt(pipelines.CodeNodeLost, message))
		}
	}
	return verdict{conclusion: ConclusionFailure, workCode: pipelines.CodeNodeLost, workMessage: message}
}

// adoptRows makes the work run's step ROWS this drive's steps, in their plan
// order: what a drive reports and settles when it has no plan of its own to
// lay over them.
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
			step:    pipelines.Step{Key: row.Key, Stage: stage, Name: name, Kind: pipelines.StepCommand},
			attempt: max(row.Attempt, 1),
			sent:    row.Status == WorkStepRunning,
			state:   StepState{Key: row.Key, Stage: stage, Name: name, Status: StepPending},
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
// for is ABANDONED here, and the runner is told to stop it (conclude).
func (dr *runDriver) reopenFromRows(rows []WorkStep) bool {
	dr.adoptRows(rows)
	decls := make([]workjournal.StepDecl, 0, len(dr.tracks))
	for _, t := range dr.tracks {
		decls = append(decls, workjournal.StepDecl{Key: t.step.Key, Kind: workjournal.KindDeterministic, StepType: "exec"})
		if t.sent && !t.finished() {
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
	step    pipelines.Step
	attempt int
	// sent: the rows say running with no receipt -- a previous driver handed
	// it to the runner, which may still be at it. Re-sent with the same
	// attempt when the run goes on; when the run ends instead, the runner is
	// told to stop it.
	sent bool
	// handle is the step's journal handle once its intent is written. Only
	// the goroutine settling the step touches it.
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
				state: StepState{Key: step.Key, Stage: step.Stage, Name: step.Name, Status: StepPending},
			}
			st.tracks = append(st.tracks, t)
			dr.tracks = append(dr.tracks, t)
		}
		dr.stages = append(dr.stages, st)
	}
}

// execute runs the stages strictly in the order written (decision 2): a
// stage's steps at once, at most maxConcurrentSteps in flight; the first
// stage that fails blocks every later stage's steps (pipeline_stage_blocked)
// -- except a notify stage's, which runs: announcing the failure is what it
// is for (D16). The block keeps naming the stage that failed first. A cancel
// stops it between stages and inside one, a notify stage's included: one the
// cancel reaches before it hands its notification over sends nothing. A lost
// lease stops it with nothing more written.
func (dr *runDriver) execute(ctx context.Context) {
	blockedBy := ""
	for i, st := range dr.stages {
		if dr.lease.isLost() {
			return
		}
		if dr.lease.isCancelled() {
			break
		}
		if blockedBy != "" && !announces(st) {
			for _, t := range st.tracks {
				if !t.finished() {
					dr.settle(ctx, t, skipReceipt(pipelines.CodeStageBlocked, "Not run: stage "+blockedBy+" failed."))
				}
			}
			continue
		}
		dr.runStage(ctx, st)
		if dr.lease.isLost() {
			return
		}
		if dr.lease.isCancelled() {
			break
		}
		if blockedBy == "" && stageFailed(st) {
			blockedBy = st.name
		}
		if i < len(dr.stages)-1 {
			dr.progress(ctx)
		}
	}
	if dr.lease.isCancelled() {
		for _, t := range dr.tracks {
			if !t.finished() {
				dr.settle(ctx, t, cancelReceipt())
			}
		}
	}
}

// runStage runs one stage's unfinished steps at once and returns when every
// one has ended or the drive has stopped.
func (dr *runDriver) runStage(ctx context.Context, st stageTracks) {
	slots := make(chan struct{}, maxConcurrentSteps)
	var wg sync.WaitGroup
	for _, t := range st.tracks {
		if t.finished() {
			continue
		}
		wg.Add(1)
		go func(t *stepTrack) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-dr.lease.stop:
				return // stopped before it started: the cancel path settles it
			}
			defer func() { <-slots }()
			dr.runStep(ctx, t)
		}(t)
	}
	wg.Wait()
}

// stageFailed reports a stage that did not pass: a step failed, was refused,
// or was cancelled by its runner (the run itself was not).
func stageFailed(st stageTracks) bool {
	for _, t := range st.tracks {
		switch t.snapshot().Status {
		case StepFailed, StepRefused, StepCancelled:
			return true
		}
	}
	return false
}

// announces reports a notify stage: one whose every step is a notify step,
// which runs after an earlier stage failed rather than being blocked by it.
func announces(st stageTracks) bool {
	if len(st.tracks) == 0 {
		return false
	}
	for _, t := range st.tracks {
		if t.step.Kind != pipelines.StepNotify {
			return false
		}
	}
	return true
}

// runStep takes one step from pending to its receipt.
func (dr *runDriver) runStep(ctx context.Context, t *stepTrack) {
	if dr.lease.isLost() || dr.lease.isCancelled() {
		return
	}
	step := t.step
	switch {
	case step.Skip != nil:
		dr.settle(ctx, t, skipReceipt(step.Skip.Code, step.Skip.Reason))
		return
	case step.Kind == pipelines.StepNotify:
		// The driver delivers a notification itself, over the outbound path
		// (notify.go): a notify step is never the step runner's.
		dr.runNotify(ctx, t)
		return
	}

	// The intent, before anything the step does.
	if !dr.stillHolds(ctx) {
		return
	}
	t.handle = dr.work().Step(ctx, step.Key)
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
	end := dr.execStep(exec, dr.request(t, secrets), dr.stepDeadline(ceiling))
	switch end.kind {
	case endLost:
		return
	case endCancelled:
		dr.settle(ctx, t, cancelReceipt())
	case endTimeout:
		dr.settle(ctx, t, failReceipt(pipelines.CodeRunCeiling, fmt.Sprintf(
			"The step did not report before its run's %s ceiling and the %s the runner is given past it, so the driver stopped waiting for it.",
			ceiling, stepGrace)))
	case endError:
		dr.settle(ctx, t, failReceipt(pipelines.CodeExecutorError,
			"The runner could not report how the step ended: "+dr.mask(end.err.Error())))
	default:
		dr.settle(ctx, t, dr.receiptFor(end.result, end.elapsed))
	}
}

// settle writes a step's receipt -- its intent first, for a step that ends
// without having run, so every declared step reads as begun and ended -- and
// records it in memory, the artifacts it names included: a notify stage later
// in the run lists them. On a lost lease it writes nothing.
func (dr *runDriver) settle(ctx context.Context, t *stepTrack, rec receipt) {
	if !dr.stillHolds(ctx) {
		return
	}
	if t.handle == nil {
		t.handle = dr.work().Step(ctx, t.step.Key)
	}
	t.handle.Finish(ctx, rec.journal())
	s := t.snapshot()
	s.Status, s.Code, s.Message, s.DurationMs, s.LogTail = rec.report, rec.code, rec.message, rec.durationMs, rec.logTail
	s.ArtifactFileIDs = slices.Clone(rec.artifactFileIDs)
	t.set(s)
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
	started, err := time.Parse(time.RFC3339, dr.facts.startedAt)
	if err != nil {
		started = dr.d.now()
	}
	return started.Add(ceiling + stepGrace).Sub(dr.d.now())
}

// requestFacts are the run's facts every StepRequest carries, fixed once the
// work run is open, so a step's goroutine never reads the drive's row.
type requestFacts struct {
	runID, workRunID, pipelineID, owner string
	attempt                             int
	startedAt                           string
	repository                          pipelines.Repository
	sha, version                        string
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
		sha: dr.run.SHA, version: version, mode: dr.run.Mode, event: dr.run.Event,
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
	return pipelines.StepRequest{
		RunID:          f.runID,
		WorkRunID:      f.workRunID,
		StepKey:        t.step.Key,
		Attempt:        t.attempt,
		RunAttempt:     f.attempt,
		RunStartedAt:   f.startedAt,
		PipelineID:     f.pipelineID,
		OwnerUserID:    f.owner,
		Repository:     f.repository,
		SHA:            f.sha,
		Mode:           f.mode,
		Event:          f.event,
		Version:        f.version,
		Domain:         f.domain,
		InstallationID: f.installation,
		Compute:        f.compute,
		Step:           t.step,
		Secrets:        secrets,
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
func (dr *runDriver) receiptFor(res pipelines.StepResult, elapsed time.Duration) receipt {
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
		case StepPending, StepRunning:
			// A step that never ended never passed.
			return verdict{conclusion: ConclusionFailure, workMessage: "Step " + s.Key + " never reported how it ended."}
		}
	}
	return verdict{conclusion: ConclusionSuccess}
}

// conclude ends the run: the work run closed, the check run completed, the
// run row completed -- in that order, so a replica that stops half-way leaves
// the run unfinished, to be recovered and concluded again, rather than a work
// run open that no sweep closes (a pipeline's run is its runner's own). Only
// the row's write is under the gate, as one fresh read and one write: the
// work run's close and the calls to GitHub come before it, outside, behind
// the fresh-read fence every unguarded write of a drive takes, while the
// heartbeat keeps the lease (drive). A successful FULL run then teaches the
// pipeline its packages' timings. It answers whether the run is concluded.
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
		dr.cancelRunner()
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

	// The fence again, before the writes that take no gate: settling the
	// stored work above may be what found the lease gone.
	if !dr.stillHolds(ctx) {
		return false
	}
	switch v.conclusion {
	case ConclusionSuccess:
		dr.work().Succeeded(ctx, map[string]any{"conclusion": v.conclusion, "stages": stageList(stages)})
	case ConclusionCancelled:
		dr.work().Cancelled(ctx, v.workCode, v.workMessage)
	default:
		dr.work().Failed(ctx, v.workCode, v.workMessage)
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

// progress records the stages as they stand and moves the check run.
func (dr *runDriver) progress(ctx context.Context) {
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
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&r.Status, p.Status)
	set(&r.Conclusion, p.Conclusion)
	set(&r.RefusalCode, p.RefusalCode)
	set(&r.RefusalMessage, p.RefusalMessage)
	set(&r.RefusalScope, p.RefusalScope)
	set(&r.CheckRunState, p.CheckRunState)
	set(&r.WorkRunID, p.WorkRunID)
	set(&r.WorkGoalID, p.WorkGoalID)
	set(&r.DriverNodeID, p.DriverNodeID)
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
