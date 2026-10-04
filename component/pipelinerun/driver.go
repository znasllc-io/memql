package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	// stepGrace is how long past a step's own timeout the driver waits for
	// the runner to report before it stops waiting (pipeline_step_timeout).
	stepGrace = 10 * time.Minute
	// executorCancelTimeout bounds Executor.Cancel.
	executorCancelTimeout = 30 * time.Second
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
	dr.drive(ctx)
}

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
// lease beside it; a cancel stops the runner's in-flight work at once.
func (dr *runDriver) drive(ctx context.Context) {
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

	v, ok := dr.steer(ctx)
	if ok && v.conclusion == ConclusionCancelled {
		dr.cancelRunner()
	}
	stopBeat()
	beating.Wait()
	if ok {
		dr.conclude(ctx, v)
	}
	close(watched)
	if !dr.lease.isLost() {
		dr.cancelExec()
	}
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
	if p == nil {
		return refusedWith(pipelines.Refuse(pipelines.CodeDisconnected, "",
			"This run's pipeline no longer exists, so nothing can run it. Connect the source's pipeline again and re-run.")), true
	}
	dr.p, dr.havePipeline = *p, true
	if !p.Active() && dr.run.Status == StatusQueued {
		// A run already started runs to its end; one that has not, does not.
		return refusedWith(pipelines.Refuse(pipelines.CodeDisconnected, "",
			"This run's pipeline was disconnected before the run started, so it was not run. Reconnect the pipeline to run this commit again.")), true
	}
	if dr.lease.isCancelled() {
		return verdict{conclusion: ConclusionCancelled, workMessage: "The run was cancelled."}, true
	}

	// The grant: every token is minted through the owner's connection, which
	// proves it still reaches the repository.
	token, installation, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
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

	// The plan.
	plan, refusal := dr.readPlan(ctx)
	if dr.lease.isLost() {
		return verdict{}, false
	}
	if refusal != nil {
		return refusedWith(refusal), true
	}
	if dr.lease.isCancelled() {
		return verdict{conclusion: ConclusionCancelled, workMessage: "The run was cancelled."}, true
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
	if errors.Is(err, errLeaseLost) {
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
	decls := dr.decls()

	if strings.TrimSpace(dr.run.WorkRunID) != "" {
		rows, err := d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), dr.run.WorkRunID)
		if err != nil {
			dr.abort("reading the work run's steps to resume it", err)
			return nil, false
		}
		if len(rows) > 0 {
			w := d.Journal.Reopen(dr.p.OwnerUserID, dr.run.WorkGoalID, dr.run.WorkRunID, decls, dr.run.StartedAt)
			if w == nil {
				dr.abort("reopening the run's work run", errors.New("the run row names no work goal to reopen"))
				return nil, false
			}
			dr.workRun.Store(w)
			dr.setFacts()
			if why := dr.resume(rows); why != "" {
				v := dr.diverged(ctx, rows, why)
				return &v, true
			}
			return nil, true
		}
		// A work run with no step rows never queued its steps: opening it
		// again writes the same ids, and nothing has run to lose.
	}

	if !dr.stillHolds(ctx) {
		return nil, false
	}
	w, err := d.Journal.Begin(ctx, workjournal.Work{
		OwnerUserID: dr.p.OwnerUserID,
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
		Steps:        decls,
		QueueSteps:   true,
		RequestedVia: "pipeline",
		TriggeredBy:  pipelines.WorkTriggerPrefix + string(dr.run.Mode),
	})
	if err != nil || w == nil {
		if err == nil {
			err = errors.New("the journal opened nothing")
		}
		dr.abort("opening the run's work goal", err)
		return nil, false
	}
	dr.workRun.Store(w)
	if err := dr.writeRun(ctx, RunPatch{
		WorkRunID: ptr(w.RunID()), WorkGoalID: ptr(w.GoalID()), Stages: ptr(dr.stageSummaries()),
	}); err != nil {
		dr.abort("recording the run's work run", err)
		return nil, false
	}
	dr.setFacts()
	return nil, true
}

// decls are the plan's steps as the journal declares them, in plan order: an
// exec step each, deterministic, waiting on the previous stage's steps. The
// call names the manifest step and, for a step that selects packages, the
// slice it was given -- names only, never a secret value -- so a resumed
// driver re-sends the slice the plan began with.
func (dr *runDriver) decls() []workjournal.StepDecl {
	out := make([]workjournal.StepDecl, 0, len(dr.tracks))
	for _, t := range dr.tracks {
		call := map[string]any{"construct": "pipeline", "name": t.step.Name, "stage": t.step.Stage}
		if len(t.step.Packages) > 0 {
			call["packages"] = slices.Clone(t.step.Packages)
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
// a step with a receipt keeps it and is not run again, a step running with no
// receipt is re-sent with the same attempt, and a step given packages runs
// THAT slice. It answers why the run cannot be resumed, or "": a step the
// rows hold and the plan does not is work the run began and could no longer
// account for.
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
		if len(row.Packages) > 0 {
			t.step.Packages = slices.Clone(row.Packages)
			t.step.Skip = nil
		}
		switch {
		case row.Finished():
			t.set(stateFromRow(t.snapshot(), row))
		case row.Status == WorkStepRunning:
			t.resend = true
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
// work began: the steps are the ROWS', and every one without a receipt fails
// pipeline_node_lost -- the driver that knew the plan is gone -- so the run
// fails rather than report a pass for work it can no longer account for.
func (dr *runDriver) diverged(ctx context.Context, rows []WorkStep, why string) verdict {
	dr.adoptRows(rows)
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
	dr.adoptRows(rows)
	// The declarations the journal needs to address each row are the rows'
	// own: an intent written with them restates the step's type, kind and
	// place, and leaves its call and dependencies as the plan wrote them.
	decls := make([]workjournal.StepDecl, 0, len(dr.tracks))
	for _, t := range dr.tracks {
		decls = append(decls, workjournal.StepDecl{Key: t.step.Key, Kind: workjournal.KindDeterministic, StepType: "exec"})
	}
	w := dr.d.Journal.Reopen(dr.run.OwnerUserID, dr.run.WorkGoalID, dr.run.WorkRunID, decls, dr.run.StartedAt)
	if w == nil {
		dr.abort("reopening the run's work run to close it", errors.New("the run row names no work goal to reopen"))
		return false
	}
	dr.workRun.Store(w)
	for _, t := range dr.tracks {
		if !t.finished() {
			dr.settle(ctx, t, rec)
		}
	}
	return true
}

// stateFromRow is a step's state as its row's receipt says it ended.
func stateFromRow(s StepState, row WorkStep) StepState {
	s.Code, s.DurationMs = row.ErrorCode, row.DurationMs
	s.Message = row.ErrorMessage
	if s.Message == "" {
		s.Message = row.Reason
	}
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
	// resend: the rows say running with no receipt -- a previous driver sent
	// it, and the runner may still be at it.
	resend bool
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
// stage that fails blocks every later stage's steps (pipeline_stage_blocked).
// A cancel stops it between stages and inside one; a lost lease stops it
// with nothing more written.
func (dr *runDriver) execute(ctx context.Context) {
	blockedBy := ""
	for i, st := range dr.stages {
		if dr.lease.isLost() {
			return
		}
		if dr.lease.isCancelled() {
			break
		}
		if blockedBy != "" {
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
		if stageFailed(st) {
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
		dr.settle(ctx, t, skipReceipt(pipelines.CodeNotifyUnavailable, fmt.Sprintf(
			"Nothing was sent to channel %s: notify stages deliver once channels arrive (epic memql#5480).", step.Channel)))
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

	end := dr.execStep(exec, dr.request(t, secrets), stepDeadline(step))
	switch end.kind {
	case endLost:
		return
	case endCancelled:
		dr.settle(ctx, t, cancelReceipt())
	case endTimeout:
		dr.settle(ctx, t, failReceipt(pipelines.CodeStepTimeout, fmt.Sprintf(
			"The step did not report within its %s timeout and the %s the runner is given past it, so the driver stopped waiting for it.",
			time.Duration(timeoutSeconds(step))*time.Second, stepGrace)))
	case endError:
		dr.settle(ctx, t, failReceipt(pipelines.CodeExecutorError,
			"The runner could not report how the step ended: "+dr.mask(end.err.Error())))
	default:
		dr.settle(ctx, t, dr.receiptFor(end.result, end.elapsed))
	}
}

// settle writes a step's receipt -- its intent first, for a step that ends
// without having run, so every declared step reads as begun and ended -- and
// records it in memory. On a lost lease it writes nothing.
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

// stepDeadline is a step's own timeout plus the grace the runner is given to
// report it.
func stepDeadline(step pipelines.Step) time.Duration {
	return time.Duration(timeoutSeconds(step))*time.Second + stepGrace
}

func timeoutSeconds(step pipelines.Step) int {
	if step.TimeoutSeconds > 0 {
		return step.TimeoutSeconds
	}
	return int(pipelines.DefaultStepTimeout / time.Second)
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
// metadata a person reads beside it. Every text the runner wrote is masked.
func (dr *runDriver) receiptFor(res pipelines.StepResult, elapsed time.Duration) receipt {
	rec := receipt{
		durationMs:      durationOf(res, elapsed),
		binding:         bindingOf(res.Where),
		logFileID:       strings.TrimSpace(res.LogFileID),
		artifactFileIDs: slices.Clone(res.ArtifactFileIDs),
		logTail:         res.LogTail,
	}
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
		rec.message = fmt.Sprintf("The runner answered an outcome this driver does not know (%q).", dr.mask(string(res.Status)))
	}
	if res.Failure != nil {
		rec.code, rec.message = strings.TrimSpace(res.Failure.Code), dr.mask(res.Failure.Message)
	}
	if rec.message == "" && rec.status == WorkStepFailed {
		rec.message = exitMessage(res.ExitCode)
	}
	notes := make([]map[string]any, 0, len(res.Notes))
	for _, n := range res.Notes {
		notes = append(notes, map[string]any{"code": n.Code, "message": dr.mask(n.Message)})
	}
	metadata := map[string]any{
		"exitCode":  res.ExitCode,
		"logLines":  res.LogLines,
		"logCapped": res.LogCapped,
	}
	if len(notes) > 0 {
		metadata["notes"] = notes
	}
	rec.result = map[string]any{"status": string(res.Status), "metadata": metadata}
	dr.observe(res.Timings)
	return rec
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

// bindingOf is where a step ran, as the step's binding records it; an empty
// field is left out rather than written blank.
func bindingOf(w pipelines.Where) map[string]any {
	out := map[string]any{}
	for name, value := range map[string]string{
		"surface": w.Surface, "nodeId": w.NodeID, "jobName": w.JobName, "workerId": w.WorkerID,
	} {
		if v := strings.TrimSpace(value); v != "" {
			out[name] = v
		}
	}
	if len(w.MachineLabels) > 0 {
		out["machineLabels"] = maps.Clone(w.MachineLabels)
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

// verdictOfSteps is how the steps ended: cancelled when the run was, failed
// when any step did not pass, success otherwise.
func (dr *runDriver) verdictOfSteps() verdict {
	if dr.lease.isCancelled() {
		return verdict{conclusion: ConclusionCancelled, workMessage: "The run was cancelled."}
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
// work run's close and the call to GitHub come before it, outside, behind the
// fresh-read fence every unguarded write of a drive takes. A successful FULL
// run then teaches the pipeline its packages' timings.
func (dr *runDriver) conclude(ctx context.Context, v verdict) {
	if !dr.stillHolds(ctx) {
		return
	}
	if dr.work() == nil && strings.TrimSpace(dr.run.WorkRunID) != "" {
		why := "The run was cancelled before this step finished."
		if v.refusal != nil {
			why = "The run ended before this step finished: " + v.refusal.Detail
		}
		if !dr.settleStoredWork(ctx, receipt{status: WorkStepCancelled, report: StepCancelled, message: why,
			result: map[string]any{"reason": why}}) {
			return
		}
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
		w := publishCheckRun(ctx, dr.d, dr.p, final, ReportFor(dr.p, final, dr.report()))
		if w.Err != nil {
			dr.log.Warn("pipelines: the concluded run's check run was not written",
				"checkRunState", w.State, "error", dr.mask(w.Err.Error()))
		}
		written = &w
	}

	err := dr.underLease(ctx, func(gctx context.Context, current Run) error {
		if written != nil {
			if cr, changed := written.Patch(current); changed {
				patch.CheckRunID, patch.CheckRunState, patch.Notes = cr.CheckRunID, cr.CheckRunState, cr.Notes
			}
		}
		return dr.d.Store.UpdateRun(gctx, current.OwnerUserID, current.ID, patch)
	})
	if err != nil {
		dr.abort("concluding the run", err)
		return
	}
	applyRunPatch(&dr.run, patch)
	dr.log.Info("pipelines: the run concluded", "conclusion", v.conclusion, "code", v.workCode)
	if v.conclusion == ConclusionSuccess && dr.run.Mode == pipelines.ModeFull && dr.havePipeline {
		if err := dr.i.mergeTimings(ctx, dr.d, dr.p.ID, dr.runID, dr.observedTimings()); err != nil {
			dr.log.Warn("pipelines: the run's package timings were not merged into the pipeline's table", "error", err)
		}
	}
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
		if !errors.Is(err, errLeaseLost) {
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
