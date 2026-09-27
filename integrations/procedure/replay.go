package procedure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/id"
)

// replay.go -- the replay runner (epic memql#5408, plan Task 5 step 2; design
// record D4, D15, D16).
//
// A learned procedure replays its TEMPLATE -- the steps stored on the
// construct -- never its rendered source, which is the artifact a person
// reads and approves. Every DECISION is made elsewhere and is a pure
// function: the rung (component/work.DecideServe, Advance), the target
// (work.ReplayTargetFor, CheckTarget), the binding and the materialization
// (component/procedure.Bind, Materialize), the preconditions
// (proc.CheckPreconditions), the comparison (work.Compare, CompareShadow), the
// running fit (proc.PrefixFits) and the diagnosis (proc.Align). This file
// sequences them, runs what they decided through the seams app/ installed,
// and writes what happened as rows.
//
// THE ORDER IS THE DESIGN, and each stage is a function of its own:
//
//	1 load        the construct, under its OWNER, and its payload
//	2 rung        shadow compares only a shadow procedure; canary and trusted
//	              RE-DECIDE the rung at execution (DecideServe), so a
//	              procedure demoted or re-lifted since compile chose it falls
//	              back to the app instead of replaying
//	3 open        the replay run -- the record of this replay, and the run
//	              every refusal after this point is written on
//	4 target      every step replayable, the footprint's target checked
//	              BEFORE the first step (D4), a dispatcher there to run it
//	5 bindings    every free parameter bound -- from the app's own actions in
//	              shadow, from the goal's input through inputMap otherwise --
//	              and never to an empty value
//	6 preconds    the learned initiation set, probed on the target (D16)
//	7 steps       materialize, check the call reads back as its template
//	              step, dispatch under the step's idempotency key, compare,
//	              and keep the live trace fitting the procedure's model; the
//	              first mismatch STOPS the replay
//	8 finish      the run closed, the ladder moved, the reliability
//	              reinforced, the one promotion raised when the ladder
//	              proposes it, and -- for a goal the procedure did not serve --
//	              the goal handed back to the app with the partial trace
//
// A REPLAY RUN IS NEVER A RECORDING. It is opened with triggeredBy
// `procedure:<mode>`, its steps are `procedureStep`, and the corpus loader
// and the learn handler both skip it: a procedure never learns from its own
// replays, and a replay never shadows itself.
//
// IT HAS NO goalId, deliberately. integrations/work dispatches every
// `running` run that carries a goal as a template to execute; a replay run is
// executed HERE, and handed to that dispatcher it would be run a second time.
// It names the run it served, or the recording it shadowed, as its parent.
//
// THE SAME REPLAY RUN NEVER REDOES A STEP. A goal served inside a run that is
// later RESUMED -- its node died mid-replay -- executes the replay statement
// again. The replay run's id is DERIVED from the goal run and that statement,
// so the second execution finds the first one's run, skips every step whose
// receipt exists, and -- when the first one finished -- answers what it
// answered without running, counting or handing anything over twice. A step
// that was IN FLIGHT when the node died (an intent, no receipt) is re-sent
// under the same idempotency key only when it could have touched nothing but
// the replay's own workbench workspace: no production dispatcher deduplicates
// on the key, so one that could have reached the world -- a machine's file or
// command, a tool call that writes, a command that sends something out -- is
// not sent again. The replay stops there and hands the goal back, naming it as
// a step that MAY HAVE RUN (review finding I5).

// Preconditions is component/procedure's learned initiation set -- what a
// Prober is handed and answers -- named here so a node that implements the
// seam from app/ needs no direct import of the pure module for its one type.
type Preconditions = proc.Preconditions

// ReplayMode is what a replay is for.
type ReplayMode string

const (
	// ReplayShadow compares the procedure beside the app, which served.
	ReplayShadow ReplayMode = "shadow"
	// ReplayCanary serves the goal with the app standing by.
	ReplayCanary ReplayMode = "canary"
	// ReplayTrusted serves the goal with no model.
	ReplayTrusted ReplayMode = "trusted"
)

// ReplayRequest is one replay.
type ReplayRequest struct {
	OwnerUserId string
	ConstructId string
	// Mode is shadow, or -- for serving a goal -- canary or trusted. For the
	// two serving modes the CURRENT rung decides which one runs: a request
	// to serve is re-decided at execution, never trusted from compile.
	Mode ReplayMode
	// GoalRunId is, for canary and trusted, the goal's run the replay serves;
	// for shadow, the recording run the replay is compared against. It is
	// the replay run's parent.
	GoalRunId string
	// Input is the goal's input (canary, trusted): each free parameter is
	// bound from it through the procedure's inputMap.
	Input map[string]any
	// Bindings are, for shadow, every hole's value as the app's own actions
	// bound it (component/procedure.BindInstance).
	Bindings map[string]string
	// AppActions are, for shadow, what the app's action at each template step
	// reported -- the reference CompareShadow holds the replay to.
	AppActions []work.StepObservation
	// AppArgs are, for shadow, the app's own arguments at each template step,
	// written relative to its workspace. A machine-local procedure's shadow
	// runs DRY -- nothing is dispatched on anybody's machine -- and compares
	// the calls it would have made with these.
	AppArgs []map[string]any
	// Unfit is, for shadow, why the recording is NOT an instance of the
	// procedure -- no run of its actions carries the procedure's steps, or
	// one does and a value the procedure holds differs. Set, the comparison
	// is a MISMATCH, recorded like any other, and nothing is replayed.
	Unfit string
	// UnfitStep is the first template step the recording lacked, with Unfit.
	UnfitStep int
	// StepKey is the goal run's step that asked for the replay (the
	// replayLearnedProcedure statement). It keys the replay run, so a resumed
	// goal run finds the replay it already started, and it names the step the
	// app fallback's session hangs off.
	StepKey string
	// GoalId and Statement are the goal being served, when the caller has
	// them; otherwise they are read off GoalRunId when the goal has to be
	// handed back.
	GoalId    string
	Statement string
}

// ReplayOutcome is what one replay did.
type ReplayOutcome struct {
	// Served (canary, trusted): the goal was answered by the procedure.
	Served bool
	// Match (shadow): every step compared equal to what the app did.
	Match bool
	// Diverged: a step differed, or could not be run, after the start.
	Diverged bool
	// DivergedStep is the index of the step that diverged; -1 when none did.
	DivergedStep int
	// Diagnosis is one paragraph: what diverged or refused, where, and why.
	Diagnosis string
	// StartRefused: nothing ran -- the rung, a step no dispatcher runs, the
	// target, a parameter the goal cannot bind, or a precondition.
	StartRefused bool
	// Insufficient: diverged although every precondition held (D16).
	Insufficient bool
	// Completed are the steps that ran and matched, with their idempotency
	// keys -- what the app is told never to repeat -- and, flagged
	// MayHaveRun, a step that was sent and whose running is unknown (its
	// target stopped answering, or its node died with it in flight), which
	// the app is told to check before repeating.
	Completed []CompletedStep
	Fitness   proc.FitnessResult
	Alignment proc.Alignment
	// FellBack: the goal was handed to the app, and Fallback is what came
	// back -- its ChildRunId is the repaired run, a new recording.
	FellBack bool
	Fallback FallbackOutcome
	// ReplayRunId is the replay run; empty when the replay was refused
	// before one was opened.
	ReplayRunId string
	// ModelCalls is always 0: the replay itself reaches no model. What a
	// fallback spends is the app's, journaled on the app's run.
	ModelCalls int
	// Transition is how the ladder moved; the zero value when it did not.
	Transition work.Transition

	// Rung is the rung the replay found the procedure on.
	Rung work.Rung
	// Code is the replay run's errorCode when it did not serve or match:
	// procedure_not_servable, procedure_start_refused, procedure_mismatch,
	// procedure_diverged, procedure_version_replaced, and -- none of which the
	// ladder counts -- procedure_target_unavailable, no_machine_dispatcher,
	// no_workbench_dispatcher, no_prober; or, when nothing could take the goal
	// over, procedure_fallback_unavailable or procedure_fallback_failed.
	Code string
	// Preconditions is the start check, when one was made.
	Preconditions proc.PreconditionReport
	// ApprovalId is the procedurePromotion approval this replay raised, when
	// the ladder proposed one.
	ApprovalId string
	// AlreadyDone: the replay run had already finished, and this call ran,
	// counted and handed over nothing that had been done.
	AlreadyDone bool
	// Interrupted: a resumed replay found a step that was in flight when its
	// node died and could have reached beyond its workspace; it was not sent
	// again, the goal went to the app naming it as one that MAY HAVE RUN, and
	// the ladder did not count it.
	Interrupted bool
	// TargetUnavailable: the target could not finish a step for a reason
	// that says nothing about the procedure, so the replay stopped there and
	// the ladder did not count it.
	TargetUnavailable bool
	// VersionReplaced: the construct was re-lifted to another version while
	// this replay ran, so the ladder did not count it.
	VersionReplaced bool
	// NotCompared (shadow): the comparison was not made ON THIS NODE -- no
	// workbench dispatcher is installed here, or the sandbox refused the
	// first step -- so nothing was recorded and the ladder did not move. A
	// run's completion event reaches whichever replica claims it, and a
	// streak must not depend on which one that was.
	NotCompared bool
}

// Outcome codes, as the replay run's errorCode carries them.
const (
	codeNotServable         = "procedure_not_servable"
	codeStartRefused        = "procedure_start_refused"
	codeMismatch            = "procedure_mismatch"
	codeDiverged            = "procedure_diverged"
	codeNoMachineDispatcher = "no_machine_dispatcher"
	codeNoWorkbench         = "no_workbench_dispatcher"
	codeFallbackUnavailable = "procedure_fallback_unavailable"
	codeFallbackFailed      = "procedure_fallback_failed"
	codeVersionReplaced     = "procedure_version_replaced"
	// codeTargetUnavailable and codeNoProber are the target or the node, not
	// the procedure: the ladder does not count either (review finding I1).
	codeTargetUnavailable = "procedure_target_unavailable"
	codeNoProber          = "no_prober"
	// codeInterrupted is a resumed replay that stopped at a step that was in
	// flight when its node died and may have reached the world (review
	// finding I5). Not counted either: a node dying is not the procedure.
	codeInterrupted = "procedure_interrupted"
)

const (
	// replayStepType and replayConstructKind are how a replay run's step rows
	// name what they are: never an app action, so never a recording's.
	replayStepType      = "procedureStep"
	replayConstructKind = "procedure"
	// fallbackLevel is the level the hand-back asks for: repairing a replay
	// that diverged is reasoning about somebody else's partial work.
	fallbackLevel = "reasoning"
	// maxReceiptOutputBytes bounds the executor output a step receipt keeps.
	// The output is kept for a later step's data-flow hole and for resume;
	// past this it is kept as its size and digest, which says what happened
	// without filling the journal.
	maxReceiptOutputBytes = 64 << 10
)

// Replay runs one replay. An error means it could not be attempted at all --
// a construct its owner cannot read, one that holds no procedure, a malformed
// request. Everything that goes wrong once it is attempted is an OUTCOME,
// written on the replay run and, for a goal, handed to the app.
func (i *Integration) Replay(ctx context.Context, req ReplayRequest) (ReplayOutcome, error) {
	out := ReplayOutcome{DivergedStep: -1}
	req.OwnerUserId = strings.TrimSpace(req.OwnerUserId)
	req.ConstructId = strings.TrimSpace(req.ConstructId)
	switch {
	case req.OwnerUserId == "":
		return out, fmt.Errorf("procedure.replay: no owner -- a learned procedure is replayed as the person it belongs to")
	case req.ConstructId == "":
		return out, fmt.Errorf("procedure.replay: constructId is required")
	}
	switch req.Mode {
	case ReplayShadow, ReplayCanary, ReplayTrusted:
	default:
		return out, fmt.Errorf("procedure.replay: mode %q is not shadow, canary or trusted", req.Mode)
	}

	c, err := i.loadForReplay(ctx, req.OwnerUserId, req.ConstructId)
	if err != nil {
		return out, err
	}
	out.Rung = c.state.Rung
	r := &replay{
		i: i, req: req, c: c, out: &out,
		policy:   i.readPolicy(ctx, req.OwnerUserId),
		free:     map[string]string{},
		outputs:  map[int]any{},
		receipts: map[string]stepReceipt{},
	}
	r.run(ctx)
	return out, nil
}

// loaded is a construct as a replay reads it.
type loaded struct {
	row       map[string]any
	id        string
	name      string
	hash      string
	signature string
	p         Procedure
	template  proc.Template
	holes     map[string]proc.Hole
	model     *proc.ProcessTree
	learned   proc.Preconditions
	state     work.LadderState
}

// loadForReplay is stage 1: the construct under its owner, and its payload.
// The payload is DECODED -- every node, form and tree checked -- because what
// comes back is about to be executed.
func (i *Integration) loadForReplay(ctx context.Context, owner, constructId string) (*loaded, error) {
	row, err := i.constructForOwner(ctx, owner, constructId)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("procedure.replay: construct %s is not readable as %s -- a goal is served only by its owner's own learned procedure", constructId, owner)
	}
	p, err := DecodeProcedure(row["procedure"])
	if err != nil {
		return nil, fmt.Errorf("procedure.replay: construct %s holds no replayable procedure: %w", constructId, err)
	}
	learned, err := DecodePreconditions(row["preconditions"])
	if err != nil {
		return nil, fmt.Errorf("procedure.replay: construct %s: %w", constructId, err)
	}
	c := &loaded{
		row: row, id: firstNonEmpty(str(row, "id"), constructId), name: str(row, "name"),
		hash: str(row, "procedureHash"), signature: str(row, "goalSignature"),
		p: p, template: p.Template(), holes: map[string]proc.Hole{}, learned: learned,
		state: ladderStateOf(row), model: p.Model,
	}
	for _, h := range p.Holes {
		c.holes[h.Id] = h
	}
	if c.model == nil {
		// The procedure's own model is the sequence of its steps' symbols
		// (the coordinator's decision recorded in the plan's Task 2 notes);
		// a payload that lost it is still that sequence.
		symbols := make([]string, len(p.Steps))
		for n, s := range p.Steps {
			symbols[n] = s.Symbol
		}
		c.model = proc.SequenceTree(symbols)
	}
	return c, nil
}

// replay is one replay in flight.
type replay struct {
	i      *Integration
	req    ReplayRequest
	c      *loaded
	out    *ReplayOutcome
	policy work.LadderPolicy

	mode  ReplayMode // shadow, or the serving rung that actually runs
	runId string
	// derived: runId is derived from what the replay is for, so another
	// execution of the same thing can hold the same run (settle checks).
	derived bool
	dry     bool
	target  work.ReplayTarget
	d       Dispatcher
	// seam is the context the prober and the dispatcher run under: the
	// owner's actor, a forwarded authority for the owner (bindAuthority),
	// and client origin (seamContext says why).
	seam context.Context

	// resuming: a replay of this very statement had started and not
	// finished, and this one continues it from its receipts.
	resuming bool
	// ranHash is the version a resumed run was opened against, when a
	// re-lift has replaced it since (reenterReplaced).
	ranHash string
	// dryStep marks the steps a SHADOW compared dry although the rest of it
	// ran in the sandbox: commands that may send something out of it
	// (network.go). A later data-flow hole reading one of them takes the
	// app's own value, as it does in a wholly dry comparison.
	dryStep map[int]bool

	free     map[string]string
	outputs  map[int]any
	receipts map[string]stepReceipt
	// intents are a resumed run's steps that were sent and left no receipt
	// -- in flight when its node died -- by step key.
	intents map[string]stepIntent
	trace   []string
	// outcome is what the replay run's outcome field says.
	outcome map[string]any
	// ranAndDiffered: the replay stopped at a step that RAN and whose outcome
	// differed, which -- with the preconditions held -- is what D16 calls a
	// precondition that proved insufficient.
	ranAndDiffered bool
	// startRefusedMidway: the first step's dispatch was refused, which ended
	// the replay as a refused start from inside the step loop.
	startRefusedMidway bool
}

// seamContext is the context a seam runs under: the one bindAuthority bound,
// or -- for a replay that binds none (a dry comparison) -- the owner's actor.
//
// ALWAYS WITH CLIENT ORIGIN (review finding M9). A replay runs inside an
// automation, whose context carries INTERNAL origin, and a replayed MCP tool
// call made under it could reach a @serverOnly construct that the recorded
// call -- made by the app over MCP, as a client -- never could. The prober and
// the dispatcher are handed the origin of the calls they replay. Every row
// this package writes stamps internal origin itself, inline (store.go), so
// nothing here needs the automation's.
func (r *replay) seamContext(ctx context.Context) context.Context {
	if r.seam != nil {
		return r.seam
	}
	return auth.ContextWithClientOrigin(ownerActor(ctx, r.req.OwnerUserId))
}

// run is stages 2 to 8, after the one question that comes before all of them:
// has this very replay already run?
//
// RE-ENTRY IS DECIDED BEFORE THE RUNG IS RE-READ. A replay that diverged has
// usually demoted its procedure, so a statement executed again after it would
// find the procedure no longer serving -- and, deciding the rung first, hand
// the goal to the app a SECOND time, a second session doing work the first
// one already did. Its own replay run is the answer to what happened, whatever
// the ladder says now.
func (r *replay) run(ctx context.Context) {
	r.mode = r.req.Mode
	if finished := r.reenter(ctx); finished {
		return
	}
	if !r.gateRung(ctx) {
		return
	}
	if r.mode == ReplayShadow && !r.resuming && !r.comparableHere() {
		return
	}
	if !r.resuming && !r.open(ctx) {
		return
	}
	if r.mode == ReplayShadow && strings.TrimSpace(r.req.Unfit) != "" {
		// A RECORDING THE PROCEDURE DOES NOT FIT IS A MISMATCH: the app did
		// this goal some other way, and a streak that ignored it would
		// promote a procedure on the recordings it happens to fit.
		r.out.Diverged = true
		r.out.DivergedStep = r.req.UnfitStep
		r.out.Diagnosis = strings.TrimSpace(r.req.Unfit)
		r.finish(ctx)
		return
	}
	if !r.checkTarget(ctx) || !r.bindAuthority(ctx) || !r.bindParameters(ctx) || !r.checkPreconditions(ctx) {
		return
	}
	r.runSteps(ctx)
	if r.startRefusedMidway || r.out.NotCompared {
		// The first step was refused before it ran: the start refusal (or,
		// in shadow, the comparison not made) is already the whole answer.
		return
	}
	if r.out.TargetUnavailable || r.out.Interrupted {
		r.finishUncounted(ctx)
		return
	}
	r.finish(ctx)
}

// comparableHere decides, BEFORE anything is written, whether this node can
// make a shadow comparison at all. A run's completion event is broadcast, so
// the comparison runs on whichever replica claimed it -- a planner, an mcp or
// an edge node, a bff with no remote workbench -- and a node with no
// workbench dispatcher answers "not compared here": no replay run, no ladder
// event, no streak reset. A procedure's evidence must not depend on which
// replica heard the event. A comparison that needs no dispatcher -- a dry one
// for a machine-local procedure, or a recording the procedure does not fit --
// is made anywhere.
func (r *replay) comparableHere() bool {
	if strings.TrimSpace(r.req.Unfit) != "" || r.c.p.Footprint.Machine {
		return true
	}
	if r.i.dispatcherFor(work.TargetWorkbench) != nil {
		return true
	}
	r.notCompared(codeNoWorkbench, "No workbench dispatcher is installed on this node, so the comparison was not made here and the ladder is where it was.")
	return false
}

// notCompared records a shadow comparison this node could not make.
func (r *replay) notCompared(code, diagnosis string) {
	r.out.NotCompared = true
	r.out.Code = code
	r.out.Diagnosis = diagnosis
	r.i.log().Info("procedure: a shadow comparison was not made on this node",
		"constructId", r.c.id, "recording", r.req.GoalRunId, "code", code)
}

// --- stage 2: the rung --------------------------------------------------------

// gateRung decides what runs. Shadow compares only a procedure IN shadow; a
// serving request re-decides the rung at execution, because the ladder may
// have moved since compile chose this procedure -- demoted by two failed
// replays, re-lifted to a new candidate, retired -- and a procedure that no
// longer serves must not serve this goal either.
func (r *replay) gateRung(ctx context.Context) bool {
	rung := r.c.state.Rung
	if r.req.Mode == ReplayShadow {
		if rung != work.RungShadow {
			r.out.Code = codeNotServable
			r.out.Diagnosis = fmt.Sprintf("Only a procedure in shadow is compared beside the app, and this one is %s.", rungPhrase(rung))
			if r.resuming {
				// A comparison that started before the procedure left shadow
				// is closed saying why, rather than left `running` for the
				// abandoned sweep to find.
				r.outcome = r.baseOutcome()
				r.outcome["diagnosis"] = r.out.Diagnosis
				r.closeRun(ctx, "failed", codeNotServable, r.out.Diagnosis)
			}
			return false
		}
		r.mode = ReplayShadow
		return true
	}
	v := work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung})
	if v.Source != work.ServeConstruct {
		reason := strings.TrimSpace(v.Reason)
		if reason == "" {
			reason = fmt.Sprintf("the procedure is %s, which does not serve a goal", rungPhrase(rung))
		}
		r.out.Code = codeNotServable
		r.out.Diagnosis = "It was chosen for this goal and no longer serves it: " + reason + "."
		if r.resuming {
			// A replay of this statement had STARTED -- its node died
			// mid-way -- and the procedure stopped serving since. The steps
			// its receipts record ran, so the app is told them, and the run
			// that started them is closed saying why it went no further.
			r.handBackResumed(ctx)
			return false
		}
		// Nothing about the procedure was attempted: no replay run is opened
		// and the ladder does not move -- the rung it already moved to is the
		// whole of the story.
		r.out.StartRefused = true
		r.fallBack(ctx, Guidance{Diagnosis: r.out.Diagnosis}, false)
		return false
	}
	r.mode = ReplayTrusted
	if v.Standby {
		r.mode = ReplayCanary
	}
	return true
}

// --- stage 3: the replay run --------------------------------------------------

// reenter finds the replay run this replay already opened, when its id is
// derived. It reports finished when that run had FINISHED: its answer is
// rebuilt from the row, and nothing runs, counts or is handed over a second
// time. A run that started and did not finish is RESUMED from its receipts.
func (r *replay) reenter(ctx context.Context) (finished bool) {
	runId, derived := r.replayRunId()
	if !derived {
		return false
	}
	r.derived = true
	actorCtx := ownerActor(ctx, r.req.OwnerUserId)
	existing, err := r.i.runForOwner(actorCtx, runId)
	if err != nil {
		r.i.log().Warn("procedure: could not read a replay run back; replaying afresh", "run", runId, "error", err)
		return false
	}
	if existing == nil {
		return false
	}
	r.runId, r.out.ReplayRunId = runId, runId
	if terminalRunStatus(str(existing, "status")) {
		r.reenterFinished(ctx, existing)
		return true
	}
	if ran := ranVersion(existing); ran != "" && ran != r.c.hash {
		r.reenterReplaced(ctx, ran)
		return true
	}
	r.resuming = true
	r.loadReceipts(actorCtx)
	return false
}

// ranVersion is the procedure version a replay run was opened against.
func ranVersion(run map[string]any) string {
	return firstNonEmpty(str(obj(run, "input"), "procedureHash"), str(run, "templateVersion"))
}

// reenterReplaced closes a replay that started against a version the
// construct no longer is. Its receipts are that version's steps, and resuming
// would hold the current template to them -- so nothing runs and nothing is
// counted: a comparison is closed as not made, and a served goal goes to the
// app with every step the receipts say ran.
func (r *replay) reenterReplaced(ctx context.Context, ran string) {
	r.out.VersionReplaced = true
	r.ranHash = ran
	r.out.Code = codeVersionReplaced
	r.out.Diagnosis = "A replay of this procedure started on a version a re-lift has since replaced, so it was closed rather than resumed against the new one."
	r.i.log().Info("procedure: a replay run of a replaced version was closed rather than resumed",
		"construct", r.c.id, "run", r.runId, "ran", ran, "current", r.c.hash)
	if r.mode == ReplayShadow {
		r.out.NotCompared = true
		r.outcome = r.baseOutcome()
		r.outcome["procedureHash"] = ran
		r.outcome["versionReplaced"] = true
		r.outcome["replacedBy"] = r.c.hash
		r.outcome["notCompared"] = true
		r.outcome["diagnosis"] = r.out.Diagnosis
		r.closeRun(ctx, "failed", r.out.Code, r.out.Diagnosis)
		return
	}
	r.resuming = true
	r.loadReceipts(ownerActor(ctx, r.req.OwnerUserId))
	r.handBackResumed(ctx)
}

// handBackResumed closes a resumed replay the procedure can no longer finish
// and hands the goal to the app with every step the receipts say ran.
func (r *replay) handBackResumed(ctx context.Context) {
	ran := r.ranSteps()
	r.out.Completed = append(r.out.Completed, ran...)
	r.out.Completed = append(r.out.Completed, r.inFlightSteps()...)
	started := len(r.out.Completed) > 0
	r.out.StartRefused = !started
	// It stopped at the first step with no receipt: a step that MAY HAVE RUN
	// did not finish.
	r.out.DivergedStep = len(ran)
	r.outcome = r.baseOutcome()
	r.outcome["diagnosis"] = r.out.Diagnosis
	r.outcome["code"] = r.out.Code
	r.outcome["completed"] = completedList(r.out.Completed)
	r.outcome["stoppedAt"] = len(ran)
	if r.out.VersionReplaced {
		r.outcome["procedureHash"] = r.ranHash
		r.outcome["versionReplaced"] = true
		r.outcome["replacedBy"] = r.c.hash
	}
	r.closeRun(ctx, "failed", r.out.Code, r.out.Diagnosis)
	r.fallBack(ctx, Guidance{Diagnosis: r.out.Diagnosis, Completed: r.out.Completed}, started)
	r.recordHandover(ctx)
}

// open opens a new replay run and reports whether it did.
func (r *replay) open(ctx context.Context) bool {
	r.runId, r.derived = r.replayRunId()
	r.out.ReplayRunId = r.runId
	actorCtx := ownerActor(ctx, r.req.OwnerUserId)
	args := map[string]any{
		"runId":               r.runId,
		"automationName":      r.c.name,
		"templateFingerprint": firstNonEmpty(r.c.hash, r.c.name),
		"templateConstructId": r.c.id,
		"triggeredBy":         replayTriggerPrefix + string(r.mode),
		"mode":                "live",
		"status":              "running",
		"startedAt":           r.now(),
		"input": map[string]any{
			"constructId":   r.c.id,
			"procedureHash": r.c.hash,
			"mode":          string(r.mode),
		},
	}
	if r.c.hash != "" {
		args["templateVersion"] = r.c.hash
	}
	if r.c.signature != "" {
		args["goalSignature"] = r.c.signature
	}
	if parent := strings.TrimSpace(r.req.GoalRunId); parent != "" {
		args["parentRunId"] = parent
	}
	if r.mode != ReplayShadow && len(r.req.Input) > 0 {
		args["variables"] = r.req.Input
	}
	if err := r.i.store.writeInternal(actorCtx, "mutation "+call("createWorkRun", args)); err != nil {
		// Without its run a replay has nowhere to record what it did, and a
		// replay nobody can read afterwards is not one the ladder may count.
		r.i.log().Warn("procedure: could not open the replay run; not replaying", "construct", r.c.id, "error", err)
		r.runId, r.out.ReplayRunId = "", ""
		r.out.StartRefused = true
		r.out.Code = codeStartRefused
		r.out.Diagnosis = "Its replay could not be recorded (" + err.Error() + "), so it did not start."
		if r.mode != ReplayShadow {
			r.fallBack(ctx, Guidance{Diagnosis: r.out.Diagnosis}, false)
		}
		return false
	}
	return true
}

// replayRunId is the replay run's id: DERIVED from what the replay is for when
// that is known -- the goal run and its statement, or the recording and the
// procedure it is compared against -- so a second execution of the same thing
// finds the first one's run; fresh otherwise.
func (r *replay) replayRunId() (string, bool) {
	scope := map[string]any{"construct": memql.BareShortId(r.c.id)}
	switch {
	case r.mode == ReplayShadow && strings.TrimSpace(r.req.GoalRunId) != "":
		// NOT the version: a recording is evidence about a procedure once. A
		// comparison asked for again after a re-lift finds the one it already
		// made, rather than counting the recording a second time against a
		// version it may have been learned into.
		scope["kind"] = "procedureShadow"
		scope["recording"] = memql.BareShortId(r.req.GoalRunId)
	case r.mode != ReplayShadow && strings.TrimSpace(r.req.GoalRunId) != "" && strings.TrimSpace(r.req.StepKey) != "":
		scope["kind"] = "procedureReplay"
		scope["run"] = memql.BareShortId(r.req.GoalRunId)
		scope["step"] = strings.TrimSpace(r.req.StepKey)
	default:
		return "v1:work:run:" + id.NewShortId(), false
	}
	return "v1:work:run:" + string(id.New().MustFromMap(scope)), true
}

func terminalRunStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "cancelled", "abandoned":
		return true
	}
	return false
}

// stepIntent is a step a replay run sent and has no receipt for: its node
// died with it in flight, or its target stopped answering. Whether it ran is
// unknown.
type stepIntent struct {
	index          int
	tool           string
	idempotencyKey string
}

// stepReceipt is a step a replay run already ran, as its receipt says. It
// carries what a resume needs to go on -- the observation to compare and the
// output a later data-flow hole reads -- and what the app must be told about
// it if the goal is handed back: the call, its key and whether it reached
// anything.
type stepReceipt struct {
	observation work.StepObservation
	output      any
	completed   CompletedStep
}

// ranSteps are the steps the loaded receipts say ran, in order -- read off
// the receipts themselves rather than the current template, whose steps a
// replay of another version need not share.
func (r *replay) ranSteps() []CompletedStep {
	out := make([]CompletedStep, 0, len(r.receipts))
	for _, rec := range r.receipts {
		out = append(out, rec.completed)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out
}

// inFlightSteps are a resumed run's intents that could have reached beyond
// the replay's own workspace, as steps that MAY HAVE RUN -- what a hand-back
// after an interruption tells the app to check before repeating. An intent
// that could only have touched the workbench's workspace is not listed: its
// effects never reached the app, whatever they were.
func (r *replay) inFlightSteps() []CompletedStep {
	target := r.target
	if target == "" {
		target = r.intendedTarget()
	}
	var out []CompletedStep
	for key, in := range r.intents {
		if _, done := r.receipts[key]; done {
			continue
		}
		args, known := r.bestEffortArgs(in.index, in.tool)
		sends := in.tool == "exec" // a command nobody can read back may send
		if known && in.tool == "exec" {
			sends, _ = sendsOutside(args)
		}
		if !r.mayHaveReachedOutside(target, in.tool, sends) {
			continue
		}
		summary := firstNonEmpty(in.tool, "a step")
		if known {
			summary = stepSummary(in.tool, args)
		}
		out = append(out, CompletedStep{Index: in.index, Tool: in.tool, IdempotencyKey: in.idempotencyKey, Summary: summary, MayHaveRun: true})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Index < out[b].Index })
	return out
}

// bestEffortArgs writes a step's call out again for a hand-back, from what a
// resumed run knows without running anything: the goal's input, the
// constants, and the outputs its receipts kept. False when any hole cannot be
// bound, or the step is not the one the intent was for (another version's).
func (r *replay) bestEffortArgs(idx int, tool string) (map[string]any, bool) {
	if r.out.VersionReplaced || idx < 0 || idx >= len(r.c.template.Steps) || r.c.template.Steps[idx].Tool != tool {
		return nil, false
	}
	step := r.c.template.Steps[idx]
	values := map[string]string{}
	ok := true
	walkHoleIds(step.Args, func(holeId string) {
		if _, done := values[holeId]; !ok || done {
			return
		}
		h, known := r.c.holes[holeId]
		switch {
		case !known:
			ok = false
		case h.Class == proc.HoleConstant:
			values[holeId] = h.Const
		case h.Class == proc.HoleDataFlow:
			if h.Ref == nil {
				ok = false
				return
			}
			rec, has := r.receipts[replayStepKey(h.Ref.StepIndex)]
			v, found := outputValueAt(rec.output, h.Ref.Path)
			if !has || !found {
				ok = false
				return
			}
			values[holeId] = v
		default:
			if v, bound := r.free[holeId]; bound {
				values[holeId] = v
				return
			}
			lit, litOK := proc.InputLiteral(r.req.Input[r.c.p.InputMap[holeId]])
			if !litOK || strings.TrimSpace(lit) == "" {
				ok = false
				return
			}
			values[holeId] = lit
		}
	})
	if !ok {
		return nil, false
	}
	raw, err := proc.Materialize(step.Args, values)
	if err != nil {
		return nil, false
	}
	args, _ := raw.(map[string]any)
	return args, args != nil
}

// mayHaveReachedOutside reports whether a step that was sent and never
// answered for could have changed something beyond the replay's own workspace
// -- which decides whether a resume may send it again (review finding I5). A
// shadow never reaches outside: it runs in the sandbox, a command that may
// send is compared dry, and its tool calls are queries. Served, a read
// changes nothing wherever it ran; a tool call may write a row, and which do
// is not the runner's to tell; and a command or a write reaches the world on
// a machine, or from anywhere when the command sends something out.
func (r *replay) mayHaveReachedOutside(target work.ReplayTarget, tool string, sends bool) bool {
	if r.mode == ReplayShadow {
		return false
	}
	switch tool {
	case "fs_read", "fetch":
		return false
	case "mcp":
		return true
	}
	return target == work.TargetMachine || sends
}

// intendedTarget is where a serving replay's steps run, decided as
// checkTarget decides it -- for the hand-back paths that stop before it runs.
func (r *replay) intendedTarget() work.ReplayTarget {
	if r.mode == ReplayShadow {
		return work.TargetWorkbench
	}
	return procedureTarget(r.c.p)
}

// procedureTarget is where a procedure replays when it serves: the target the
// lift stored, or -- for a payload that stored none it can read -- the one its
// footprint names (D4).
func procedureTarget(p Procedure) work.ReplayTarget {
	if stored, ok := parseTarget(p.Target); ok {
		return stored
	}
	return work.ReplayTargetFor(p.Footprint)
}

// loadReceipts reads the steps a resumed replay run already ran. A step with a
// receipt is NEVER dispatched again. One left at `running` -- or failed
// because its target stopped answering, which is the same unknown -- is an
// INTENT: sent, and whether it ran is unknown (runStep decides whether it may
// be sent again).
func (r *replay) loadReceipts(actorCtx context.Context) {
	rows, err := r.i.store.query(actorCtx, "query "+call("workStepsForOwnerRun", map[string]any{"runId": r.runId}))
	if err != nil {
		r.i.log().Warn("procedure: could not read a resumed replay's steps", "run", r.runId, "error", err)
		return
	}
	if r.intents == nil {
		r.intents = map[string]stepIntent{}
	}
	for _, row := range rows {
		key := str(row, "key")
		payload := obj(obj(row, "result"), "result")
		status := str(row, "status")
		mayHaveRun, _ := payload["mayHaveRun"].(bool)
		if status == "running" || (status == "failed" && mayHaveRun) {
			r.intents[key] = stepIntent{
				index: intOf(row, "seq"), tool: firstNonEmpty(str(obj(row, "call"), "tool"), str(payload, "tool")),
				idempotencyKey: str(row, "idempotencyKey"),
			}
			continue
		}
		if status != "done" && status != "skipped" {
			continue
		}
		rec := stepReceipt{output: payload["output"]}
		if o := payload["observation"]; o != nil {
			_ = decodeInto(o, &rec.observation)
		}
		side, _ := payload["sideEffect"].(bool)
		rec.completed = CompletedStep{
			Index: intOf(row, "seq"), Tool: str(payload, "tool"), IdempotencyKey: str(row, "idempotencyKey"),
			Summary: str(payload, "summary"), SideEffect: side,
		}
		r.receipts[key] = rec
	}
}

// reenterFinished answers a replay whose run already finished, from the run
// row, running and counting nothing -- the ladder heard of it when it
// finished. One case still acts: a serving replay whose goal was never handed
// back -- its node died between stopping and the hand-over, or the abandoned
// sweep closed it mid-flight -- hands the goal over now, because the goal
// still has to be served, and tells the app every step the receipts say ran.
func (r *replay) reenterFinished(ctx context.Context, run map[string]any) {
	o := obj(run, "outcome")
	r.adoptFinished(run)
	if r.out.FellBack || r.mode == ReplayShadow || r.out.Served {
		return
	}
	if o == nil {
		// Closed with no outcome: the run never reached its own finish. Its
		// receipts are the only record of what ran.
		r.loadReceipts(ownerActor(ctx, r.req.OwnerUserId))
		ran := r.ranSteps()
		r.out.Completed = append(r.out.Completed, ran...)
		r.out.Completed = append(r.out.Completed, r.inFlightSteps()...)
		r.out.Diagnosis = fmt.Sprintf("A replay of this goal was interrupted (its run is %s) after %d step(s), and the app takes it from there.",
			firstNonEmpty(str(run, "status"), "closed"), len(ran))
		r.out.DivergedStep = len(ran)
		o = r.baseOutcome()
		o["interrupted"] = true
		o["diagnosis"] = r.out.Diagnosis
		o["completed"] = completedList(r.out.Completed)
		o["stoppedAt"] = len(ran)
	}
	r.outcome = o
	started := r.out.Diverged || len(r.out.Completed) > 0
	if started && !r.out.Diverged {
		// Where it stopped, as the run recorded it; counting the list would
		// count a step that MAY HAVE RUN as one that finished.
		r.out.DivergedStep = len(r.out.Completed)
		if _, stored := o["stoppedAt"]; stored {
			r.out.DivergedStep = intOf(o, "stoppedAt")
		}
	}
	r.fallBack(ctx, Guidance{Diagnosis: r.out.Diagnosis, Completed: r.out.Completed}, started)
	r.recordHandover(ctx)
}

// adoptFinished answers what a finished replay run's row says it did, and
// marks the answer as one that ran, counted and handed over nothing now.
func (r *replay) adoptFinished(run map[string]any) {
	o := obj(run, "outcome")
	r.out.AlreadyDone = true
	r.out.Served = str(o, "servedBy") == "procedure"
	r.out.Match, _ = o["match"].(bool)
	r.out.Diverged, _ = o["diverged"].(bool)
	r.out.StartRefused, _ = o["startRefused"].(bool)
	r.out.VersionReplaced, _ = o["versionReplaced"].(bool)
	r.out.Diagnosis = str(o, "diagnosis")
	r.out.Code = str(run, "errorCode")
	r.out.DivergedStep = -1
	if r.out.Diverged {
		r.out.DivergedStep = intOf(o, "divergedStep")
	}
	r.out.Completed = completedFrom(o["completed"])
	if child := str(o, "repairRunId"); child != "" {
		r.out.FellBack = true
		r.out.Fallback = FallbackOutcome{ChildRunId: child}
	}
}

// --- stage 4: the target --------------------------------------------------------

// checkTarget refuses, before the first step, a version no dispatcher runs, a
// target the footprint cannot replay on, and a target nobody installed a
// dispatcher for.
func (r *replay) checkTarget(ctx context.Context) bool {
	if why, blocked := firstUnreplayable(r.c.template); blocked {
		return r.refuseStart(ctx, codeStartRefused, "It cannot run: "+why+".", true)
	}
	fp := r.c.p.Footprint
	if r.mode == ReplayShadow {
		// A SHADOW NEVER TOUCHES A PERSON'S MACHINE. A portable procedure is
		// compared by replaying it in the sandboxed workbench; a
		// machine-local one runs DRY -- nothing is dispatched anywhere -- and
		// the calls it would make are compared with the app's own.
		if fp.Machine {
			r.dry = true
			return true
		}
		r.target = work.TargetWorkbench
	} else {
		r.target = r.intendedTarget()
		// BEFORE THE FIRST STEP (D4): a machine-local procedure that failed at
		// step three on the workbench has already run steps one and two
		// somewhere they mean nothing.
		if err := work.CheckTarget(fp, r.target); err != nil {
			reason := err.Error()
			if errors.Is(err, work.ErrMachineLocalOnWorkbench) {
				reason = "its footprint names files outside the workspace, on the machine it was recorded on, and it was sent to the workbench"
			}
			return r.refuseStart(ctx, codeStartRefused, "It cannot replay on the "+string(r.target)+": "+reason+".", true)
		}
	}
	r.d = r.i.dispatcherFor(r.target)
	if r.d == nil {
		code := codeNoWorkbench
		if r.target == work.TargetMachine {
			code = codeNoMachineDispatcher
		}
		// The NODE is missing a seam, which says nothing about the
		// procedure, so the ladder does not count it.
		return r.refuseStart(ctx, code, fmt.Sprintf("No %s dispatcher is installed on the node that served this goal (%s), so nothing ran.", r.target, code), false)
	}
	return true
}

// bindAuthority is the context the seams run under: the owner's actor, and a
// FORWARDED AUTHORITY for the owner. The workbench is reached over the mesh
// when it runs on its own node, and that forward fails closed without an
// assertion of whose work it is. A served replay runs inside the goal's work
// dispatch, which already bound one for the run's owner, and it is kept; a
// shadow comparison runs from the completion trigger with only the borrowed
// owner, so it is given the writer-scoped assertion the work spine gives
// background work -- no captured grant, so never more than a writer.
func (r *replay) bindAuthority(ctx context.Context) bool {
	if r.dry {
		return true
	}
	owner := r.req.OwnerUserId
	if fa, ok := auth.ForwardedAuthorityFromContext(ctx); ok && sameUser(fa.Subject, owner) {
		r.seam = auth.ContextWithClientOrigin(ownerActor(ctx, owner))
		return true
	}
	bound, err := auth.ContextWithPersistedOwner(ctx, owner, nil, nil)
	if err != nil {
		return r.refuseStart(ctx, codeStartRefused, "The owner's authority could not be bound for the replay's steps: "+err.Error()+".", false)
	}
	r.seam = auth.ContextWithClientOrigin(bound)
	return true
}

func parseTarget(s string) (work.ReplayTarget, bool) {
	switch t := work.ReplayTarget(strings.TrimSpace(s)); t {
	case work.TargetWorkbench, work.TargetMachine:
		return t, true
	}
	return "", false
}

// --- stage 5: the bindings ------------------------------------------------------

// bindParameters binds every free parameter before anything runs. A parameter
// the goal cannot supply refuses the start: a replay NEVER runs with a hole
// bound to nothing, because it would do something nobody asked for.
func (r *replay) bindParameters(ctx context.Context) bool {
	for _, h := range r.c.p.Holes {
		if h.Class != proc.HoleFree && h.Class != proc.HoleUnexplained {
			continue
		}
		if r.mode == ReplayShadow {
			v, ok := r.req.Bindings[h.Id]
			if !ok {
				return r.refuseStart(ctx, codeStartRefused, fmt.Sprintf("The app's own actions bound no value for parameter %s, so there is nothing to compare.", h.Id), false)
			}
			r.free[h.Id] = v
			continue
		}
		key, mapped := r.c.p.InputMap[h.Id]
		if !mapped {
			return r.refuseStart(ctx, codeStartRefused, fmt.Sprintf("Parameter %s is supplied by no goal input, so no goal can bind it.", h.Id), true)
		}
		raw, present := r.req.Input[key]
		lit, ok := proc.InputLiteral(raw)
		if !present || !ok || strings.TrimSpace(lit) == "" {
			return r.refuseStart(ctx, codeStartRefused, fmt.Sprintf("The goal's input %q gives parameter %s no value, and a replay never runs with a parameter nobody supplied.", key, h.Id), true)
		}
		r.free[h.Id] = lit
	}
	// WHAT A VALUE MEANS is judged before anything runs. Strict quoting makes
	// every value one argument; a value shaped like nothing the recordings put
	// there -- an option, an absolute path, a parent directory -- is still a
	// call no recording made (component/procedure.CheckBindings).
	if err := proc.CheckBindings(r.c.template, r.free); err != nil {
		sentence := err.Error()
		if sentence != "" {
			sentence = strings.ToUpper(sentence[:1]) + sentence[1:]
		}
		if r.mode == ReplayShadow {
			// The app's own action bound it, so the RECORDING is not an
			// instance of the procedure: a MISMATCH, recorded like an unfit
			// recording's, with nothing replayed.
			r.out.Diverged = true
			r.out.DivergedStep = 0
			var refusal *proc.BindingRefusal
			if errors.As(err, &refusal) {
				r.out.DivergedStep = refusal.Step
			}
			r.out.Diagnosis = sentence + ", so the app did this goal some other way."
			r.finish(ctx)
			return false
		}
		return r.refuseStart(ctx, codeStartRefused, sentence+".", true)
	}
	return true
}

// --- stage 6: the preconditions ------------------------------------------------

// checkPreconditions probes the target for what the procedure learned and
// refuses the start when it does not hold (D16). An ABSENT measurement is
// never a match: with no prober, a learned predicate the target is compared
// on is unmeasured, and does not hold.
func (r *replay) checkPreconditions(ctx context.Context) bool {
	if r.dry {
		return true
	}
	target := string(r.target)
	var (
		observed proc.Preconditions
		probeErr error
		code     = codeTargetUnavailable
	)
	if needsProbe := !proc.CheckPreconditions(r.c.learned, proc.Preconditions{}, target).Held; needsProbe {
		if p := r.i.prober(); p != nil {
			stopPulse := r.pulseWhile(ctx)
			observed, probeErr = p.Probe(r.seamContext(ctx), r.target, r.req.OwnerUserId, r.runId, r.c.learned)
			stopPulse()
		} else {
			probeErr, code = errors.New("no prober is installed on this node"), codeNoProber
		}
	}
	report := proc.CheckPreconditions(r.c.learned, observed, target)
	if probeErr != nil {
		report.Held = false
	}
	r.out.Preconditions = report
	if report.Held {
		return true
	}
	if probeErr != nil {
		// A PROBE THAT COULD NOT RUN measured nothing -- the machine asleep,
		// no workbench peer, no prober on this node -- so it says nothing
		// about whether the preconditions hold, and the ladder does not count
		// it (review finding I1). The goal still goes to the app.
		return r.refuseStart(ctx, code,
			"Its preconditions could not be checked on the "+target+": "+preconditionSentence(report, probeErr)+".", false)
	}
	// A SHADOW that cannot start is evidence of nothing: the app served the
	// goal regardless, and the ladder is left where it was.
	return r.refuseStart(ctx, codeStartRefused,
		"Its preconditions did not hold on the "+target+": "+preconditionSentence(report, nil)+".", r.mode != ReplayShadow)
}

func preconditionSentence(report proc.PreconditionReport, probeErr error) string {
	var parts []string
	if len(report.Mismatches) > 0 {
		parts = append(parts, strings.Join(report.Mismatches, "; "))
	}
	if len(report.Unmeasured) > 0 {
		parts = append(parts, "not measured: "+strings.Join(report.Unmeasured, ", "))
	}
	if probeErr != nil {
		parts = append(parts, "the probe failed: "+probeErr.Error())
	}
	if len(parts) == 0 {
		return "no reason was reported"
	}
	return strings.Join(parts, "; ")
}

// refuseStart ends a replay before its first step and records why on the
// replay run. counts decides whether the ladder hears of it: a canary or
// trusted procedure whose start was refused has failed a goal it was chosen
// for; a missing seam, or a shadow, has not. The goal -- when there is one --
// goes to the app.
func (r *replay) refuseStart(ctx context.Context, code, diagnosis string, counts bool) bool {
	r.out.StartRefused = true
	r.out.Code = code
	r.out.Diagnosis = diagnosis
	r.outcome = r.baseOutcome()
	r.outcome["startRefused"] = true
	r.outcome["diagnosis"] = diagnosis
	r.outcome["code"] = code
	if m := r.out.Preconditions.Mismatches; len(m) > 0 {
		r.outcome["mismatches"] = append([]string(nil), m...)
	}
	if u := r.out.Preconditions.Unmeasured; len(u) > 0 {
		r.outcome["unmeasured"] = append([]string(nil), u...)
	}
	serving := r.mode != ReplayShadow
	if serving && r.i.appFallback() == nil {
		r.out.Code = codeFallbackUnavailable
		r.outcome["fallback"] = "no app fallback is installed on this node, so nothing served the goal"
	}
	var ev *work.LadderEvent
	if counts && serving {
		ev = &work.LadderEvent{Kind: work.EventStartRefused, At: r.i.clock().UTC()}
	}
	if !r.settle(ctx, "failed", r.out.Code, diagnosis, ev, false, false) {
		return false
	}
	if serving {
		r.fallBack(ctx, Guidance{Diagnosis: diagnosis}, false)
		r.recordHandover(ctx)
	}
	return false
}

// --- stage 7: the steps ----------------------------------------------------------

// runSteps replays the template one step at a time, and stops at the first
// step that does not match.
func (r *replay) runSteps(ctx context.Context) {
	for idx := range r.c.template.Steps {
		if !r.runStep(ctx, idx) {
			return
		}
	}
	r.out.Fitness = proc.TokenReplay(r.c.model, r.trace)
	if !r.out.Fitness.Fits {
		// Every step ran and matched, and the procedure's own model still
		// does not accept the run: the stored steps and model disagree, and
		// a replay of something that inconsistent is not one to count.
		r.diverge(len(r.c.template.Steps), "", fmt.Sprintf(
			"Every step ran and matched, and the procedure's own model does not accept the run (fitness %.2f), so the stored procedure is inconsistent.",
			r.out.Fitness.Fitness), false)
	}
}

// runStep replays one step and reports whether the replay goes on.
//
// STEPS ARE NUMBERED FROM 1 IN EVERY SENTENCE A PERSON READS -- a diagnosis,
// a refusal, the guidance handed to the app -- because MemQL OS lists a
// procedure's steps from 1, and "step 0 did not match" beside that list sends
// a person to the wrong step. Every STORED index (divergedStep, a receipt's
// seq, CompletedStep.Index) and every key (stepN, the idempotency key) stays
// the 0-based position it always was.
func (r *replay) runStep(ctx context.Context, idx int) bool {
	step := r.c.template.Steps[idx]
	key := replayStepKey(idx)
	symbol := r.symbolOf(idx)
	idem := work.IdempotencyKey(r.runId, key, 1)

	values, err := r.holeValues(idx, step)
	if err != nil {
		return r.diverge(idx, idem, fmt.Sprintf("Step %d (%s) could not be bound: %v.", idx+1, oneLine(step.Tool), err), false)
	}
	raw, err := proc.Materialize(step.Args, values)
	if err != nil {
		return r.diverge(idx, idem, fmt.Sprintf("Step %d (%s) could not be written out: %v.", idx+1, oneLine(step.Tool), err), false)
	}
	args, _ := raw.(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	summary := stepSummary(step.Tool, args)
	if ok, why := inputHolds(r.c.template, idx, step.Tool, args, values); !ok {
		return r.diverge(idx, idem, fmt.Sprintf("Step %d (%s: %s) would not have made the call the procedure names: %s.", idx+1, step.Tool, summary, why), false)
	}

	done := CompletedStep{Index: idx, Tool: step.Tool, IdempotencyKey: idem, Summary: summary}
	var (
		obs    work.StepObservation
		output any
	)
	// A COMMAND THAT MAY SEND SOMETHING OUT OF THE SANDBOX (network.go) is
	// never dispatched by a shadow -- the workbench has full egress, and a
	// recorded webhook post or push would be sent again beside every
	// recording that matches it. It is compared dry, as a machine-local
	// procedure's whole comparison is. Served, it runs, and is a side effect
	// the app must not repeat whatever the workbench says it delivered.
	sends := false
	if step.Tool == "exec" {
		sends, _ = sendsOutside(args)
	}
	stepDry := r.dry
	if r.mode == ReplayShadow && sends && !r.dry {
		stepDry = true
		if r.dryStep == nil {
			r.dryStep = map[int]bool{}
		}
		r.dryStep[idx] = true
	}
	receipt, ran := r.receipts[key]
	if _, inFlight := r.intents[key]; inFlight && !ran && !stepDry && r.mayHaveReachedOutside(r.target, step.Tool, sends) {
		// SENT, AND NEVER ANSWERED FOR: the node died with this step in
		// flight. It could have reached the world, and no dispatcher
		// deduplicates on the idempotency key -- so it is not sent again.
		return r.stopInterrupted(ctx, idx, done)
	}
	switch {
	case ran:
		// A RECEIPT EXISTS: this step ran before this replay run was resumed.
		// It is never dispatched again; what it reported then is compared now.
		obs, output = receipt.observation, receipt.output
		done.SideEffect = receipt.completed.SideEffect
	case stepDry:
		r.writeStepIntent(ctx, idx, key, idem)
	default:
		r.writeStepIntent(ctx, idx, key, idem)
		stopPulse := r.pulseWhile(ctx)
		res, derr := r.d.Dispatch(r.seamContext(ctx), DispatchRequest{
			Target: r.target, OwnerUserId: r.req.OwnerUserId, RunId: r.runId,
			StepKey: key, IdempotencyKey: idem, Tool: step.Tool, Args: args,
			Sandbox: r.mode == ReplayShadow, Timeout: r.stepTimeout(idx),
		})
		stopPulse()
		if derr != nil {
			// A GO ERROR MEANS THE STEP DID NOT RUN -- a gate or scope
			// refusal, a missing surface, a spelling the executor does not
			// support, the sandbox saying no, the machine not there. Before
			// anything has run that is the TARGET refusing, which says nothing
			// about the procedure: a refused start the ladder does not count
			// (review finding I1), and in shadow a comparison not made here.
			// After a step ran, it is a divergence that delivered nothing.
			r.writeStepReceipt(ctx, key, "failed", done, nil, nil, "procedure_step_failed", derr.Error(), false, "")
			if len(r.out.Completed) == 0 {
				if r.mode == ReplayShadow {
					r.notCompared(codeTargetUnavailable, fmt.Sprintf("Its first step (%s: %s) was refused before it ran on this node (%v), so the comparison was not made.", step.Tool, summary, derr))
					r.outcome = r.baseOutcome()
					r.outcome["notCompared"] = true
					r.outcome["diagnosis"] = r.out.Diagnosis
					r.closeRun(ctx, "failed", codeTargetUnavailable, r.out.Diagnosis)
					return false
				}
				r.refuseStart(ctx, codeTargetUnavailable, fmt.Sprintf("Its first step (%s: %s) was refused before it ran: %v.", step.Tool, summary, derr), false)
				r.startRefusedMidway = true
				return false
			}
			return r.diverge(idx, idem, fmt.Sprintf("Step %d (%s: %s) was refused before it ran: %v.", idx+1, step.Tool, summary, derr), false)
		}
		if res.Unavailable {
			return r.stopUnavailable(ctx, idx, key, done, res)
		}
		obs, output = res.Observation, res.Output
		// A SIDE EFFECT is what the DISPATCHER says reached the world beyond
		// the replay's own workspace -- a machine's file or command, an MCP
		// call that wrote -- and the app is told never to repeat it. A step
		// that only touched the workbench's workspace delivered nothing the
		// app shares: it runs in a workspace of its own -- unless the command
		// itself sends something out of it (network.go), which reaches the
		// world from the workbench as surely as from a machine.
		done.SideEffect = res.Delivered || sends
	}

	match, why := r.compare(idx, step.Tool, args, obs)
	if !ran {
		status := "done"
		if stepDry {
			status = "skipped"
		}
		r.writeStepReceipt(ctx, key, status, done, &obs, output, "", "", match, strings.Join(why, "; "))
	}
	if match {
		r.trace = append(r.trace, symbol)
	} else {
		r.trace = append(r.trace, symbol+"!")
	}
	if fits, _ := proc.PrefixFits(r.c.model, r.trace); !match || !fits {
		reason := strings.Join(why, "; ")
		if reason == "" {
			reason = "the procedure's own model does not allow this step here"
		}
		what := "what every recording did"
		if r.mode == ReplayShadow {
			what = "what the app did"
		}
		// INSUFFICIENT (D16) is reserved for a step that RAN TO AN ANSWER of
		// its own and answered differently while every precondition held. A
		// step that timed out, or lost its answer, did not finish: what it
		// would have said is unknown, and that is an ordinary failure.
		return r.diverge(idx, idem, fmt.Sprintf("Step %d (%s: %s) did not match %s: %s.", idx+1, step.Tool, summary, what, reason),
			!match && completedStep(step.Tool, obs, output))
	}
	r.outputs[idx] = output
	r.out.Completed = append(r.out.Completed, done)
	return true
}

// stepTimeout is the time limit a step is dispatched with: the longest any
// recording of it asked for (payload.go's hints), or zero -- the executor's
// own default -- when none did.
func (r *replay) stepTimeout(idx int) time.Duration {
	h := r.c.p.Hints
	if h == nil || idx >= len(h.TimeoutsMs) || h.TimeoutsMs[idx] <= 0 {
		return 0
	}
	ms := int64(h.TimeoutsMs[idx])
	if ms > math.MaxInt64/int64(time.Millisecond) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms) * time.Millisecond
}

// compare holds one step to its reference: what every recording agreed on
// (canary, trusted), the app beside it (shadow), or -- for a dry shadow, or a
// shadow step compared dry because it may send something out -- the app's own
// call.
func (r *replay) compare(idx int, tool string, args map[string]any, obs work.StepObservation) (bool, []string) {
	if r.dry || r.dryStep[idx] {
		if idx >= len(r.req.AppArgs) || r.req.AppArgs[idx] == nil {
			return false, []string{"the app's call at this step was not recorded, so a dry comparison has nothing to hold it to"}
		}
		got, want := canonicalCall(tool, args), canonicalCall(tool, r.req.AppArgs[idx])
		if got == nil || want == nil || !got.Equal(want) {
			return false, []string{"the call it would make is not the call the app made"}
		}
		return true, nil
	}
	var exp work.StepExpectation
	if idx < len(r.c.p.Expect) {
		exp = r.c.p.Expect[idx]
	}
	if r.mode == ReplayShadow {
		var app work.StepObservation
		if idx < len(r.req.AppActions) {
			app = r.req.AppActions[idx]
		}
		return work.CompareShadow(exp, app, obs)
	}
	return work.Compare(exp, obs)
}

// completedStep reports whether a step RAN TO AN ANSWER of its own: the
// executor reported what it did, success or error. A step that stopped short
// of one -- a timeout, a signal, a lost reply -- reports an error with the
// measurement it never took (a command's exit code, a fetch's or a tool's
// result type) missing, or names the timeout; what it would have answered is
// unknown, so its difference from the recordings is no evidence about the
// procedure's preconditions.
func completedStep(tool string, obs work.StepObservation, output any) bool {
	if obs.IsError == nil {
		return false
	}
	if !*obs.IsError {
		return true
	}
	if m, ok := output.(map[string]any); ok {
		switch str(m, "errorCode") {
		case "timeout", "worker_disconnected":
			return false
		}
	}
	switch tool {
	case "exec":
		return obs.ExitCode != nil
	case "fetch", "mcp":
		return obs.ResultType != ""
	}
	// A write or a read that failed is the executor's answer about the file.
	return true
}

// stopUnavailable stops a replay at a step its target could not finish for a
// reason that says nothing about the procedure (review finding I1): the
// stream dropped, a forward failed, no workbench peer answered. The step MAY
// HAVE RUN -- its effects are unknown -- so it is recorded that way and the
// app is told to check before repeating it. The ladder does not count it: a
// shadow comparison is not made, and a served goal goes to the app.
func (r *replay) stopUnavailable(ctx context.Context, idx int, key string, done CompletedStep, res DispatchResult) bool {
	done.MayHaveRun = true
	done.SideEffect = res.Delivered
	why := "the target stopped answering"
	if m, ok := res.Output.(map[string]any); ok {
		if code := strings.TrimSpace(str(m, "errorCode")); code != "" {
			why = firstNonEmpty(strings.TrimSpace(str(m, "errorMessage")), code) + " (" + code + ")"
		}
	}
	diagnosis := fmt.Sprintf("Step %d (%s: %s) could not be finished on the %s -- %s -- so whether it ran is unknown.",
		idx+1, done.Tool, done.Summary, r.targetName(), why)
	r.writeStepReceipt(ctx, key, "failed", done, &res.Observation, res.Output, codeTargetUnavailable, diagnosis, false, "")
	if r.mode == ReplayShadow {
		r.notCompared(codeTargetUnavailable, diagnosis+" The comparison was not made, and the ladder is where it was.")
		r.outcome = r.baseOutcome()
		r.outcome["notCompared"] = true
		r.outcome["diagnosis"] = r.out.Diagnosis
		r.closeRun(ctx, "failed", codeTargetUnavailable, r.out.Diagnosis)
		return false
	}
	r.out.TargetUnavailable = true
	r.out.Code = codeTargetUnavailable
	r.out.DivergedStep = idx
	r.out.Diagnosis = diagnosis
	r.out.Completed = append(r.out.Completed, done)
	return false
}

// stopInterrupted stops a resumed replay at a step that was in flight when its
// node died and could have reached the world (review finding I5). It is not
// sent again; the goal goes to the app naming it as one that MAY HAVE RUN,
// and the ladder does not count it -- a node dying is not the procedure.
func (r *replay) stopInterrupted(ctx context.Context, idx int, done CompletedStep) bool {
	done.MayHaveRun = true
	diagnosis := fmt.Sprintf("A replay of this goal was interrupted while step %d (%s: %s) was in flight, and whether it ran is unknown; "+
		"it could have reached beyond the replay's own workspace, so it was not sent again.", idx+1, done.Tool, done.Summary)
	if r.mode == ReplayShadow {
		r.notCompared(codeInterrupted, diagnosis)
		r.outcome = r.baseOutcome()
		r.outcome["notCompared"] = true
		r.outcome["diagnosis"] = r.out.Diagnosis
		r.closeRun(ctx, "failed", codeInterrupted, r.out.Diagnosis)
		return false
	}
	r.out.Interrupted = true
	r.out.Code = codeInterrupted
	r.out.DivergedStep = idx
	r.out.Diagnosis = diagnosis
	r.out.Completed = append(r.out.Completed, done)
	return false
}

// targetName is where the replay's steps run, as a sentence says it.
func (r *replay) targetName() string {
	if r.target == "" {
		return "target"
	}
	return string(r.target)
}

// finishUncounted closes a served replay that stopped for a reason that says
// nothing about the procedure -- its target stopped answering, or its node
// died with a step in flight -- and hands the goal to the app with every step
// that ran or may have. No ladder event: the procedure is not what failed.
func (r *replay) finishUncounted(ctx context.Context) {
	r.outcome = r.divergenceOutcome()
	r.outcome["code"] = r.out.Code
	r.outcome["diagnosis"] = r.out.Diagnosis
	r.outcome["stoppedAt"] = r.out.DivergedStep
	if r.out.TargetUnavailable {
		r.outcome["targetUnavailable"] = true
	}
	if r.out.Interrupted {
		r.outcome["interrupted"] = true
	}
	if r.i.appFallback() == nil {
		r.out.Code = codeFallbackUnavailable
		r.outcome["fallback"] = "no app fallback is installed on this node, so nothing served the goal"
	}
	if !r.settle(ctx, "failed", r.out.Code, r.out.Diagnosis, nil, false, false) {
		return
	}
	r.fallBack(ctx, Guidance{Diagnosis: r.out.Diagnosis, Completed: append([]CompletedStep(nil), r.out.Completed...)}, true)
	r.recordHandover(ctx)
}

// holeValues is every value one step's holes take: a free parameter's
// binding, a constant's recorded value, and a data-flow hole's value read out
// of the earlier step's output -- or, where that step was compared dry and so
// produced nothing, the app's.
func (r *replay) holeValues(idx int, step proc.TemplateStep) (map[string]string, error) {
	values := map[string]string{}
	var walkErr error
	walkHoleIds(step.Args, func(holeId string) {
		if walkErr != nil {
			return
		}
		if _, done := values[holeId]; done {
			return
		}
		h, known := r.c.holes[holeId]
		if !known {
			walkErr = fmt.Errorf("hole %s is not one the procedure declares", holeId)
			return
		}
		switch h.Class {
		case proc.HoleConstant:
			values[holeId] = h.Const
		case proc.HoleDataFlow:
			if h.Ref == nil || h.Ref.StepIndex >= idx {
				walkErr = fmt.Errorf("hole %s names no earlier step", holeId)
				return
			}
			if r.dry || r.dryStep[h.Ref.StepIndex] {
				if v, ok := r.req.Bindings[holeId]; ok {
					values[holeId] = v
					return
				}
			}
			v, ok := outputValueAt(r.outputs[h.Ref.StepIndex], h.Ref.Path)
			if !ok {
				walkErr = fmt.Errorf("step %d's output carries no value at %s for hole %s",
					h.Ref.StepIndex+1, strings.Join(h.Ref.Path, "."), holeId)
				return
			}
			values[holeId] = v
		default:
			v, ok := r.free[holeId]
			if !ok {
				walkErr = fmt.Errorf("parameter %s is unbound", holeId)
				return
			}
			values[holeId] = v
		}
	})
	return values, walkErr
}

// walkHoleIds calls fn for every hole in a tree, in order.
func walkHoleIds(n *proc.Node, fn func(string)) {
	if n == nil {
		return
	}
	if n.Kind == proc.KindHole {
		fn(n.HoleId)
		return
	}
	for _, k := range n.Kids {
		walkHoleIds(k, fn)
	}
}

// outputValueAt reads a data-flow value out of a step's output: object keys
// and list indices, down to a scalar spelled as InputLiteral spells a goal
// input -- the one spelling a hole's value has.
func outputValueAt(v any, path []string) (string, bool) {
	cur := v
	for _, seg := range path {
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			n, err := strconv.Atoi(seg)
			if err != nil || n < 0 || n >= len(t) {
				return "", false
			}
			cur = t[n]
		default:
			return "", false
		}
	}
	return proc.InputLiteral(cur)
}

// inputHolds is the content-addressed input check (D16): the call about to be
// dispatched must read back -- canonicalized exactly as a recording is -- as
// an instance of its template step, with every hole taking the value the
// replay bound. A step whose recorded arguments were identical in every
// recording has no holes, so the check is that it is the very call they
// made; a step with holes must bind back to the same values. A
// materialization that re-quoted an argument into a different one, or a
// stored payload whose tree and forms disagree, is refused here, before
// anything runs.
func inputHolds(t proc.Template, idx int, tool string, args map[string]any, values map[string]string) (bool, string) {
	acts := proc.Canonicalize([]proc.Step{{StepType: tool, Input: args, Consumed: true}})
	if len(acts) != 1 {
		return false, "its arguments do not canonicalize to one call"
	}
	bound, ok := proc.Bind(t, idx, acts[0])
	if !ok {
		return false, "its arguments do not read back as the procedure's step"
	}
	for holeId, want := range values {
		if got, present := bound[holeId]; !present || got != want {
			return false, fmt.Sprintf("hole %s reads back as %q, not %q", holeId, got, want)
		}
	}
	return true, ""
}

// canonicalCall is a call's arguments as the corpus canonicalizes them.
func canonicalCall(tool string, args map[string]any) *proc.Node {
	acts := proc.Canonicalize([]proc.Step{{StepType: tool, Input: args, Consumed: true}})
	if len(acts) != 1 {
		return nil
	}
	return acts[0].Args
}

// diverge stops the replay at a step. ranAndDiffered says the step RAN and its
// outcome differed -- with the preconditions held, D16's precondition that
// proved insufficient; a step that could not be bound, written out or run at
// all is a plain failure.
func (r *replay) diverge(idx int, idem, diagnosis string, ranAndDiffered bool) bool {
	r.out.Diverged = true
	r.out.DivergedStep = idx
	r.ranAndDiffered = ranAndDiffered
	if len(r.trace) <= idx && idx < len(r.c.template.Steps) {
		// The step never produced an outcome of its own; the trace records
		// the deviation at the position it would have taken.
		r.trace = append(r.trace, r.symbolOf(idx)+"!")
	}
	r.out.Alignment = proc.Align(r.c.model, r.trace)
	r.out.Fitness = proc.TokenReplay(r.c.model, r.trace)
	if m, ok := r.out.Alignment.FirstDeviation(); ok {
		diagnosis += " " + alignmentSentence(m)
	}
	if idem != "" {
		diagnosis += " Its idempotency key is " + idem + "."
	}
	r.out.Diagnosis = strings.TrimSpace(diagnosis)
	return false
}

// alignmentSentence names the first deviation in the vocabulary the diagnosis
// handed to the app uses.
func alignmentSentence(m proc.Move) string {
	switch m.Kind {
	case proc.MoveLog:
		return fmt.Sprintf("Against the procedure's own model the first deviation is a log move -- the run did something the procedure does not (%s) -- at position %d.", m.Label, m.TraceIndex+1)
	case proc.MoveModel:
		return fmt.Sprintf("Against the procedure's own model the first deviation is a model move -- the procedure has a step the run did not take (%s) -- at position %d.", m.Label, m.TraceIndex+1)
	}
	return ""
}

// --- stage 8: the finish ----------------------------------------------------------

// finish closes the run and moves the ladder. The RUN IS CLOSED FIRST: it is
// the record that this replay happened, and the ladder is derived from what
// happened. A node that dies between the two undercounts -- the direction
// that never promotes on evidence nobody can read afterwards. Both happen in
// ONE critical section (settle), and the goal, when the procedure did not
// serve it, is handed back only after it -- the app's session can run for
// minutes, and nothing about it needs the lock.
func (r *replay) finish(ctx context.Context) {
	if r.mode == ReplayShadow {
		r.finishShadow(ctx)
		return
	}
	now := r.i.clock().UTC()
	if !r.out.Diverged {
		r.out.Served = true
		r.outcome = r.divergenceOutcome()
		r.outcome["servedBy"] = "procedure"
		r.outcome["steps"] = len(r.c.template.Steps)
		r.settle(ctx, "succeeded", "", "", &work.LadderEvent{Kind: work.EventReplayed, At: now, Match: true}, true, true)
		return
	}
	r.out.Insufficient = r.ranAndDiffered
	r.out.Code = codeDiverged
	r.outcome = r.divergenceOutcome()
	r.outcome["code"] = codeDiverged
	if r.i.appFallback() == nil {
		r.out.Code = codeFallbackUnavailable
		r.outcome["fallback"] = "no app fallback is installed on this node, so nothing served the goal"
	}
	ev := work.LadderEvent{Kind: work.EventReplayed, At: now, Match: false, Insufficient: r.out.Insufficient}
	if !r.settle(ctx, "failed", r.out.Code, r.out.Diagnosis, &ev, true, false) {
		return
	}
	r.fallBack(ctx, Guidance{
		Diagnosis: r.out.Diagnosis,
		Completed: append([]CompletedStep(nil), r.out.Completed...),
		Alignment: append([]proc.Move(nil), r.out.Alignment.Moves...),
	}, true)
	r.recordHandover(ctx)
}

// finishShadow closes a shadow comparison: the ladder counts it, the
// reliability hears of it, and when the streak has earned it the ONE promotion
// is raised (D3) and its id written onto the construct in the same ladder
// write, so the ladder never proposes twice.
func (r *replay) finishShadow(ctx context.Context) {
	r.out.Match = !r.out.Diverged
	r.outcome = r.divergenceOutcome()
	r.outcome["match"] = r.out.Match
	status, code, message := "succeeded", "", ""
	if !r.out.Match {
		r.out.Code = codeMismatch
		r.outcome["code"] = codeMismatch
		status, code, message = "failed", codeMismatch, r.out.Diagnosis
	}
	bindings := map[string]string{}
	for _, holeId := range r.c.p.FreeParameters {
		if v, ok := r.free[holeId]; ok {
			bindings[holeId] = v
		}
	}
	r.settle(ctx, status, code, message, &work.LadderEvent{
		Kind: work.EventShadowCompared, At: r.i.clock().UTC(), Match: r.out.Match,
		Bindings: bindings, FreeParameters: append([]string(nil), r.c.p.FreeParameters...),
	}, true, r.out.Match)
}

// settle closes the replay run and moves the ladder by ev as ONE critical
// section: under the construct's ladder lock, from the construct read FRESH
// inside it (ladder.go, review finding C1). It reports false when another
// finisher of this same replay run closed it first -- this one then answers
// what that one recorded, and must hand nothing over.
//
// Three things are decided on the fresh read, never on what the replay loaded:
//
//   - WHETHER THIS FINISH IS THE FIRST. A derived run can be finished by two
//     executions of the same statement (a resumed goal run whose first
//     executor was still alive, a comparison asked for twice at once). The
//     second finds the run closed with an outcome and counts nothing. (A run
//     the abandoned sweep closed has no outcome, and is closed again with
//     what happened.)
//   - WHETHER THE VERSION IS STILL THE ONE THAT RAN. A re-lift while the
//     replay was in flight put a NEW version on the entry rung; evidence
//     about the old one is no evidence about it, and writing the loaded state
//     back would put the new version on the old one's rung. The event is
//     dropped, and the run says why.
//   - WHERE THE LADDER STANDS. The event is advanced from the fresh state, so
//     a promotion decided, a streak advanced or a demotion made meanwhile is
//     kept -- and an event the rung no longer takes changes nothing.
//
// A lock that cannot be taken, or a construct that cannot be read, still
// closes the run -- the record that the replay happened -- and moves no
// ladder.
func (r *replay) settle(ctx context.Context, status, code, message string, ev *work.LadderEvent, reinforce, success bool) bool {
	fresh, release, err := r.i.lockedConstruct(ctx, r.req.OwnerUserId, r.c.id)
	if err != nil {
		if ev != nil {
			r.i.log().Warn("procedure: the ladder could not be moved after a replay; the replay is recorded and not counted",
				"construct", r.c.id, "run", r.runId, "error", err)
			if r.outcome != nil {
				r.outcome["ladderNotMoved"] = err.Error()
			}
		}
		r.closeRun(ctx, status, code, message)
		return true
	}
	defer release()
	if r.derived && r.runId != "" {
		// Finished by ANOTHER EXECUTION means closed WITH AN OUTCOME. The
		// abandoned-run sweep closes a run with none, and a run it closed
		// under a replay that was still working is closed again here with
		// what actually happened.
		if run, rerr := r.i.runForOwner(ownerActor(ctx, r.req.OwnerUserId), r.runId); rerr == nil && run != nil &&
			terminalRunStatus(str(run, "status")) && obj(run, "outcome") != nil {
			r.i.log().Info("procedure: a replay run was finished by another execution first; this one counts nothing",
				"construct", r.c.id, "run", r.runId)
			r.adoptFinished(run)
			return false
		}
	}
	if current := str(fresh, "procedureHash"); current != r.c.hash {
		if ev != nil {
			r.out.VersionReplaced = true
			if r.outcome != nil {
				r.outcome["versionReplaced"] = true
				r.outcome["replacedBy"] = current
			}
			r.i.log().Info("procedure: the procedure was re-lifted while a replay of it ran; the ladder did not count the replay",
				"construct", r.c.id, "run", r.runId, "ran", r.c.hash, "current", current)
		}
		r.closeRun(ctx, status, code, message)
		return true
	}
	r.closeRun(ctx, status, code, message)
	if ev != nil {
		r.advanceFrom(ctx, fresh, *ev, reinforce, success)
	}
	return true
}

// advanceFrom moves the ladder by one event from a construct read inside its
// lock -- raising the promotion when the move proposes one -- and reinforces
// the reliability when the event was a replay. Only a transition that changes
// something is written: an event the fresh rung no longer takes (a comparison
// of a procedure promoted meanwhile) is no move, and writing its sentence
// would overwrite the reason the rung actually moved.
func (r *replay) advanceFrom(ctx context.Context, fresh map[string]any, ev work.LadderEvent, reinforce, success bool) {
	before := ladderStateOf(fresh)
	t := work.Advance(before, ev, r.policy)
	if t.Propose {
		approvalId, err := r.i.raisePromotion(ctx, r.req.OwnerUserId, r.c, r.runId, t, r.dry || len(r.dryStep) > 0)
		if err != nil {
			// The ladder still moves -- the evidence is real -- and the
			// proposal is made again on the next match, because the state
			// written carries no open approval.
			r.i.log().Warn("procedure: the ladder proposed a promotion and it could not be raised",
				"construct", r.c.id, "error", err)
		} else {
			t.State.PromotionApprovalId = approvalId
			r.out.ApprovalId = approvalId
		}
	}
	r.out.Transition = t
	if ladderMoved(before, t) {
		if err := r.i.writeLadder(ctx, r.req.OwnerUserId, r.c.id, t); err != nil {
			r.i.log().Warn("procedure: could not write the ladder after a replay", "construct", r.c.id, "error", err)
		} else {
			r.c.state = t.State
		}
	}
	if reinforce {
		if err := r.i.reinforce(ctx, r.req.OwnerUserId, r.c.id, fresh, success); err != nil {
			r.i.log().Warn("procedure: could not reinforce after a replay", "construct", r.c.id, "error", err)
		}
	}
}

// --- the run's rows ------------------------------------------------------------------

func (r *replay) baseOutcome() map[string]any {
	return map[string]any{
		"mode":          string(r.mode),
		"constructId":   r.c.id,
		"procedureHash": r.c.hash,
		"rung":          string(r.c.state.Rung),
		"modelCalls":    0,
	}
}

func (r *replay) divergenceOutcome() map[string]any {
	o := r.baseOutcome()
	o["diverged"] = r.out.Diverged
	if r.out.Diverged {
		o["divergedStep"] = r.out.DivergedStep
		o["diagnosis"] = r.out.Diagnosis
		o["insufficient"] = r.ranAndDiffered
	}
	o["completed"] = completedList(r.out.Completed)
	return o
}

// completedList writes the completed steps as the outcome carries them, in
// the lowerCamel every other field of the run uses.
func completedList(steps []CompletedStep) []any {
	out := make([]any, 0, len(steps))
	for _, s := range steps {
		step := map[string]any{
			"index": s.Index, "tool": s.Tool, "idempotencyKey": s.IdempotencyKey,
			"summary": s.Summary, "sideEffect": s.SideEffect,
		}
		if s.MayHaveRun {
			step["mayHaveRun"] = true
		}
		out = append(out, step)
	}
	return out
}

// completedFrom reads completedList back off a stored outcome.
func completedFrom(v any) []CompletedStep {
	list, _ := v.([]any)
	out := make([]CompletedStep, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		side, _ := m["sideEffect"].(bool)
		mayHave, _ := m["mayHaveRun"].(bool)
		out = append(out, CompletedStep{
			Index: intOf(m, "index"), Tool: str(m, "tool"), IdempotencyKey: str(m, "idempotencyKey"),
			Summary: str(m, "summary"), SideEffect: side, MayHaveRun: mayHave,
		})
	}
	return out
}

// closeRun writes the replay run's terminal version.
func (r *replay) closeRun(ctx context.Context, status, code, message string) {
	if r.runId == "" {
		return
	}
	args := map[string]any{
		"runId":      r.runId,
		"status":     status,
		"finishedAt": r.now(),
		"outcome":    r.outcome,
	}
	if code != "" {
		args["errorCode"] = code
		args["errorMessage"] = message
	}
	if err := r.i.store.writeInternal(ownerActor(ctx, r.req.OwnerUserId), "mutation "+call("updateWorkRun", args)); err != nil {
		r.i.log().Warn("procedure: could not close the replay run", "run", r.runId, "error", err)
	}
}

// recordHandover writes what the hand-back did onto the replay run, once the
// app answered: the repaired run -- a new recording, which the learner reads
// like any other -- or why nothing served the goal.
func (r *replay) recordHandover(ctx context.Context) {
	if r.runId == "" || r.mode == ReplayShadow || r.outcome == nil {
		return
	}
	args := map[string]any{"runId": r.runId}
	switch {
	case r.out.FellBack:
		r.outcome["repairRunId"] = r.out.Fallback.ChildRunId
		r.outcome["servedBy"] = "app"
	case r.out.Code == codeFallbackFailed:
		args["errorCode"] = codeFallbackFailed
		args["errorMessage"] = str(r.outcome, "fallback")
	default:
		return
	}
	args["outcome"] = r.outcome
	if err := r.i.store.writeInternal(ownerActor(ctx, r.req.OwnerUserId), "mutation "+call("updateWorkRun", args)); err != nil {
		r.i.log().Warn("procedure: could not record the hand-back on the replay run", "run", r.runId, "error", err)
	}
}

// writeStepIntent writes a step's `running` version before it is dispatched,
// and a heartbeat on the run -- the abandoned sweep judges a run by it, and
// pulseWhile keeps it fresh for as long as the step is in flight.
func (r *replay) writeStepIntent(ctx context.Context, idx int, key, idem string) {
	actorCtx := ownerActor(ctx, r.req.OwnerUserId)
	now := r.now()
	tool := ""
	if idx < len(r.c.template.Steps) {
		tool = r.c.template.Steps[idx].Tool
	}
	if err := r.i.store.writeInternal(actorCtx, "mutation "+call("createWorkStep", map[string]any{
		"stepId":   r.stepId(key),
		"runId":    r.runId,
		"key":      key,
		"seq":      idx,
		"stepType": replayStepType,
		"kind":     string(work.KindDeterministic),
		// The TOOL is named on the intent, as the construct is: a resume
		// that finds the intent and no receipt must say what was in flight,
		// even when the version it ran has since been replaced.
		"call":           map[string]any{"construct": replayConstructKind, "name": r.c.name, "tool": tool},
		"status":         "running",
		"attempt":        1,
		"idempotencyKey": idem,
		"startedAt":      now,
	})); err != nil {
		r.i.log().Warn("procedure: could not write a replay step's intent", "run", r.runId, "step", key, "error", err)
	}
	r.heartbeat(ctx)
}

// heartbeat writes one heartbeat on the replay run -- the abandoned sweep
// judges a run by it.
func (r *replay) heartbeat(ctx context.Context) {
	if err := r.i.store.writeInternal(ownerActor(ctx, r.req.OwnerUserId), "mutation "+call("updateWorkRun", map[string]any{
		"runId": r.runId, "heartbeatAt": r.now(),
	})); err != nil {
		r.i.log().Debug("procedure: could not write a replay heartbeat", "run", r.runId, "error", err)
	}
}

// pulseWhile renews the replay run's heartbeat until the returned stop is
// called (review finding I3). One beat per step, before its dispatch, left a
// replay whose step ran longer than the abandoned sweep's window -- a build, a
// test suite, a download on somebody's machine -- looking abandoned while it
// worked; the probe, which runs commands on the target too, is covered the
// same way. The interval is the automation runtime's
// (component/automations/heartbeat.go), and stop JOINS the ticker, as that
// file's does: a heartbeat is a read-merge, and one still in flight when the
// receipt or the run's close is written could put a stale status back.
func (r *replay) pulseWhile(ctx context.Context) func() {
	if r.runId == "" {
		return func() {}
	}
	every := r.i.heartbeatInterval()
	pulseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-pulseCtx.Done():
				return
			case <-ticker.C:
				beatCtx, stop := context.WithTimeout(pulseCtx, 5*time.Second)
				r.heartbeat(beatCtx)
				stop()
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// writeStepReceipt writes a step's receipt: what it reported, the output a
// later step or a resume needs, whether it matched -- and the call it made,
// its tool and whether it may have touched anything, which is what a hand-back
// after an interruption tells the app not to repeat.
func (r *replay) writeStepReceipt(ctx context.Context, key, status string, done CompletedStep, obs *work.StepObservation, output any, errCode, errMsg string, passed bool, message string) {
	payload := map[string]any{
		"tool":       done.Tool,
		"summary":    done.Summary,
		"sideEffect": done.SideEffect,
	}
	if done.MayHaveRun {
		payload["mayHaveRun"] = true
	}
	if obs != nil {
		if o, err := asObject(obs); err == nil {
			payload["observation"] = o
		}
	}
	if kept := boundedOutput(output); kept != nil {
		payload["output"] = kept
	}
	args := map[string]any{
		"stepId":     r.stepId(key),
		"status":     status,
		"result":     map[string]any{"status": status, "result": payload},
		"finishedAt": r.now(),
	}
	if errMsg != "" {
		args["errorCode"] = firstNonEmpty(errCode, "procedure_step_failed")
		args["errorMessage"] = errMsg
	} else {
		args["postcondition"] = map[string]any{
			"kind": "check", "ref": "procedure.expect", "passed": passed, "message": message,
		}
	}
	if err := r.i.store.writeInternal(ownerActor(ctx, r.req.OwnerUserId), "mutation "+call("updateWorkStep", args)); err != nil {
		r.i.log().Warn("procedure: could not write a replay step's receipt", "run", r.runId, "step", key, "error", err)
	}
}

// boundedOutput is the executor output a receipt keeps: whole when it is
// small, and as its size and digest past maxReceiptOutputBytes.
func boundedOutput(output any) any {
	if output == nil {
		return nil
	}
	b, err := canonicalJSON(output)
	if err != nil {
		return nil
	}
	if len(b) <= maxReceiptOutputBytes {
		var decoded any
		if err := json.Unmarshal(b, &decoded); err == nil {
			return decoded
		}
		return nil
	}
	return map[string]any{"truncated": true, "bytes": len(b), "digest": proc.Digest(b)}
}

// stepId is a replay step's row id: the run's short id and the step key,
// composed the way the automation journal composes one, so it names both.
func (r *replay) stepId(key string) string {
	return "v1:work:step:" + memql.BareShortId(r.runId) + "-" + key
}

func replayStepKey(idx int) string { return "step" + strconv.Itoa(idx) }

func (r *replay) symbolOf(idx int) string {
	if idx < len(r.c.p.Steps) && r.c.p.Steps[idx].Symbol != "" {
		return r.c.p.Steps[idx].Symbol
	}
	return "s" + strconv.Itoa(idx)
}

func (r *replay) now() string { return r.i.clock().UTC().Format(timeLayout) }

// --- the hand-back ---------------------------------------------------------------------

// fallBack hands the goal to the app. Absent, the goal is not served and the
// outcome says why -- never a silent success. started says whether any step
// ran, which decides what the app is told.
func (r *replay) fallBack(ctx context.Context, g Guidance, started bool) {
	f := r.i.appFallback()
	if f == nil {
		r.out.Code = codeFallbackUnavailable
		return
	}
	goalId, statement := r.goalContext(ctx)
	g.Procedure = r.c.name
	g.Prompt = renderGuidance(statement, g, started, r.out.DivergedStep)
	// Every seam sees the OWNER's actor, as the dispatcher does: the goal is
	// theirs, and so is whatever the app does with it.
	fo, err := f.Handover(ownerActor(ctx, r.req.OwnerUserId), FallbackRequest{
		OwnerUserId: r.req.OwnerUserId,
		GoalId:      goalId,
		RunId:       r.req.GoalRunId,
		StepId:      journalStepId(r.req.GoalRunId, r.req.StepKey),
		App:         r.c.p.RecordedFrom.App,
		Level:       fallbackLevel,
		Statement:   statement,
		Guidance:    g,
	})
	if err != nil {
		r.out.Code = codeFallbackFailed
		if r.outcome != nil {
			r.outcome["fallback"] = "the app could not take the goal over: " + err.Error()
		}
		r.i.log().Warn("procedure: the app fallback failed", "construct", r.c.id, "run", r.req.GoalRunId, "error", err)
		return
	}
	r.out.FellBack = true
	r.out.Fallback = fo
}

// goalContext is the goal a hand-back is about: what the caller said, or what
// the goal run and its goal say.
func (r *replay) goalContext(ctx context.Context) (string, string) {
	goalId, statement := strings.TrimSpace(r.req.GoalId), strings.TrimSpace(r.req.Statement)
	if (goalId != "" && statement != "") || strings.TrimSpace(r.req.GoalRunId) == "" {
		return goalId, firstNonEmpty(statement, r.c.p.Title)
	}
	actorCtx := ownerActor(ctx, r.req.OwnerUserId)
	if goalId == "" {
		if run, err := r.i.runForOwner(actorCtx, r.req.GoalRunId); err == nil && run != nil {
			goalId = strings.TrimSpace(str(run, "goalId"))
		}
	}
	if statement == "" && goalId != "" {
		rows, err := r.i.store.query(actorCtx, "query "+call("workGoalForOwner", map[string]any{"goalId": goalId}))
		if err == nil && len(rows) > 0 {
			statement = strings.TrimSpace(str(rows[0], "statement"))
		}
	}
	return goalId, firstNonEmpty(statement, r.c.p.Title)
}

// renderGuidance is the text the app reads before it starts, appended to the
// goal statement: every step that already ran with its idempotency key, and
// where and why the replay stopped -- repair, not resample (D16). A replay
// that ran nothing says so, and that nothing has been done.
func renderGuidance(statement string, g Guidance, started bool, stoppedAt int) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(statement))
	b.WriteString("\n\n")
	if !started {
		b.WriteString("A learned procedure for this goal did not start, so nothing has been done yet: ")
		b.WriteString(g.Diagnosis)
		return b.String()
	}
	var delivered, local, unknown []CompletedStep
	for _, s := range g.Completed {
		switch {
		case s.MayHaveRun:
			unknown = append(unknown, s)
		case s.SideEffect:
			delivered = append(delivered, s)
		default:
			local = append(local, s)
		}
	}
	if len(delivered) > 0 {
		b.WriteString("A learned procedure already ran these steps -- do not repeat them:\n")
		for _, s := range delivered {
			fmt.Fprintf(&b, "- step %d (%s): %s [idempotency key %s]\n", s.Index+1, s.Tool, s.Summary, s.IdempotencyKey)
		}
	}
	if len(local) > 0 {
		// THESE ARE NOT THE APP'S TO SKIP. They ran in the replay's own
		// workspace, which the app does not share, so nothing they did
		// reached it; the app redoes whatever of them the goal still needs.
		b.WriteString("It also ran these steps in its own workspace, which you do not share, so their effects did not reach you:\n")
		for _, s := range local {
			fmt.Fprintf(&b, "- step %d (%s): %s [idempotency key %s]\n", s.Index+1, s.Tool, s.Summary, s.IdempotencyKey)
		}
	}
	if len(unknown) > 0 {
		// NEITHER DONE NOR NOT DONE. The step was sent and nothing came back
		// that says whether it ran; telling the app it did not would have it
		// repeat an effect that may have landed.
		b.WriteString("These steps were sent and whether they ran is unknown -- they may have run, so check before repeating them:\n")
		for _, s := range unknown {
			fmt.Fprintf(&b, "- step %d (%s): %s [idempotency key %s]\n", s.Index+1, s.Tool, s.Summary, s.IdempotencyKey)
		}
	}
	fmt.Fprintf(&b, "It stopped at step %d because: %s", stoppedAt+1, g.Diagnosis)
	return b.String()
}

// journalStepId is the goal run's step as the automation journal writes it --
// the canonical row id of <run short id>-<step key> -- which is what the app
// session's recording stamps childRunId onto.
func journalStepId(runId, stepKey string) string {
	runId, stepKey = strings.TrimSpace(runId), strings.TrimSpace(stepKey)
	if runId == "" || stepKey == "" {
		return ""
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '-'
	}, stepKey)
	return "v1:work:step:" + memql.BareShortId(runId) + "-" + safe
}

// stepSummary is one line saying what a step did.
func stepSummary(tool string, args map[string]any) string {
	var s string
	switch tool {
	case "exec":
		switch c := args["command"].(type) {
		case string:
			s = "ran `" + c + "`"
		case []any:
			words := make([]string, 0, len(c))
			for _, w := range c {
				words = append(words, fmt.Sprint(w))
			}
			s = "ran `" + strings.Join(words, " ") + "`"
		}
	case "fs_write":
		s = "wrote " + strings.Join(pathsInArgs(args), ", ")
	case "fs_read":
		s = "read " + strings.Join(pathsInArgs(args), ", ")
	case "fetch":
		s = "fetched " + str(args, "url")
	case "mcp":
		s = "called the MemQL tool " + str(args, "tool")
	}
	if strings.TrimSpace(s) == "" {
		s = tool
	}
	return truncateRunes(oneLine(s), 160)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// rungPhrase is a rung as a sentence says it.
func rungPhrase(r work.Rung) string {
	switch r {
	case work.RungNone:
		return "on no rung"
	case work.RungShadow:
		return "in shadow"
	case work.RungTrusted:
		return "trusted"
	case work.RungRetired:
		return "retired"
	}
	return "a " + string(r)
}

// runForOwner reads one run under the actor in ctx, or nil.
func (i *Integration) runForOwner(ctx context.Context, runId string) (map[string]any, error) {
	rows, err := i.store.query(ctx, "query "+call("workRunForOwner", map[string]any{"runId": runId}))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}
