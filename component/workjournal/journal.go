// Package workjournal writes the work spine's rows for work the SERVER
// starts on its own: a goal, one run per attempt, and one step per stage.
//
// ===========================================================================
// WHY A PACKAGE RATHER THAN A FEW CALLS WHERE THE WORK HAPPENS
// ===========================================================================
// Every mutation this makes is `@serverOnly`, and origin defaults to CLIENT
// -- so a caller that does not stamp internal origin has each write refused
// with a WARN in a log and nothing else. The stamp is allowlisted per
// PACKAGE (component/auth/call_origin.go, TestOnlyAllowlistedPackagesStamp-
// InternalOrigin), and the allowlist's own standard is that an entry should
// be "small, exists for one operation family, and every call site in it is
// downstream of one gate". That is this package and it is not
// `integrations/library`, which also serves request-derived paths like
// `libraryTrainFile` where a stamp would hand a caller-scoped read the
// engine's escape.
//
// So the journal lives here, the stamp lives here, and the callers hold a
// handle. The gate this package's call sites are downstream of is stated and
// asserted in internal_origin_precondition_test.go: Begin REFUSES without an
// owner, so no row is ever written under a blank actor.
//
// ===========================================================================
// WHAT IT IS FOR, AND WHAT IT IS NOT
// ===========================================================================
// It is for work a person did not dispatch and cannot see any other way --
// today, the Library's file analysis pass (spec section G). A goal is the
// standing intent ("understand this file"), so it is keyed to the file and a
// re-analysis is a SECOND RUN of the same goal rather than a second goal.
// That is the goal/run split the design asks for, and it is what makes the
// Training app's feed read as one thing per file rather than one per attempt.
//
// It is NOT the automation executor's journal (epic A1,
// component/automations/journal.go). That one writes a run for every
// automation execution from inside the executor and owns resume. This writes
// runs for Go-driven passes that are not automations at all. They share the
// rows and nothing else.
//
// Its second writer is a RUNNER-OWNED run (epic memql#5477): a pipeline's,
// which the pipelines driver opens on an agent node with every step queued
// (Work.QueueSteps), closes one receipt at a time (Step.Finish), beats while
// it runs (Run.Heartbeat), and hands to another replica's driver when the
// first goes silent (Journal.Reopen). Its triggeredBy (Work.TriggeredBy) is
// what tells integrations/work that the run is not its to dispatch, sweep or
// re-run.
//
// NIL-SAFE THROUGHOUT. Every method tolerates a nil receiver and returns a
// nil-safe handle, so a caller wires the journal if it has one and calls it
// unconditionally either way. A pass whose journal is absent behaves exactly
// as it did before the journal existed -- which is what lets this be added
// to a working path without a branch at every call site.
package workjournal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Executor is the engine seam. Narrow on purpose: this package renders MemQL
// call strings and runs them, which is how every server-side writer in this
// tree calls a mutation.
type Executor interface {
	Execute(ctx context.Context, query string) (any, error)
}

// ExecutorFunc adapts an engine whose Execute returns a concrete result type.
//
// The journal reads NOTHING off a result -- a mutation's answer is the row it
// wrote, and the ids here are derived rather than returned -- so discarding
// it costs nothing, and the `any` in the interface is what keeps this package
// off component/memql's import graph. One line at each wiring site is a
// cheaper price than the dependency.
//
//	workjournal.New(workjournal.ExecutorFunc(func(ctx context.Context, q string) (any, error) {
//	    return engine.Execute(ctx, q)
//	}), logger, nodeID)
type ExecutorFunc func(ctx context.Context, query string) (any, error)

func (f ExecutorFunc) Execute(ctx context.Context, query string) (any, error) {
	return f(ctx, query)
}

// Kinds, per the derived-kind rule (spec section B). A stage that reaches a
// prompt is `reasoning` whatever else it does; everything else a Go pass runs
// is `deterministic`. Recording that honestly is what lets a later reader ask
// "which of these cost a model call" without re-deriving it.
const (
	KindDeterministic = "deterministic"
	KindReasoning     = "reasoning"
)

// TriggerPrefix marks a run a Go DRIVER writes and runs itself: its
// triggeredBy is `journal:<template>`.
//
// Such a run carries a goal, `running` and an automationName -- the template
// WORD the feed filters by, which names no automation anybody registered --
// and that is exactly the shape the work dispatcher takes for compiled goal
// work. Before the marker, every agent replica claimed each journal run about
// a millisecond after it opened, failed to load a template called
// "libraryAnalyzeFile" or "appSession", and failed the run
// automation_not_runnable over whatever its driver was recording. The
// dispatcher (integrations/work.IsDriverOwnedRun) refuses a run carrying this
// prefix, and so does every recovery path in its sweep.
//
// Here rather than in integrations/work because this package is the one
// every writer of such a run can import: integrations/work's own recording
// writer uses it too, so there is one spelling and not two to keep in step.
const TriggerPrefix = "journal:"

// TriggeredBy is the triggeredBy a driver-owned run of this template carries.
func TriggeredBy(template string) string {
	return TriggerPrefix + strings.TrimSpace(template)
}

// HeartbeatInterval is how often an open run says it is still being worked.
//
// The work sweep closes a `running` run whose heartbeat is older than a
// minute as abandoned by a node that went away -- and nothing else beats for a
// run a driver owns, because no executor runs it. A third of that window, so
// one lost write never closes a live pass.
const HeartbeatInterval = 20 * time.Second

// Journal writes the rows.
type Journal struct {
	engine Executor
	logger *slog.Logger
	now    func() time.Time
	nodeID string
	// beat overrides HeartbeatInterval so a test can watch several beats.
	beat time.Duration
}

// New builds a journal. A nil engine yields a journal whose methods are all
// no-ops, which is the same shape as no journal at all.
func New(engine Executor, logger *slog.Logger, nodeID string) *Journal {
	if engine == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Journal{engine: engine, logger: logger, now: time.Now, nodeID: nodeID, beat: HeartbeatInterval}
}

// Work describes the pass being opened.
type Work struct {
	// OwnerUserID is whose work this is. REQUIRED: a row written under a
	// blank actor is readable by nobody, including the operator answering
	// "where did my file go" -- the lesson memql#4354 recorded for the
	// workbench's own workspace rows.
	OwnerUserID string
	// Template names the deterministic template being run, and is what a
	// client filters the feed by. It is the run's `automationName`, which is
	// the field the concept declares for exactly this.
	Template string
	// Statement is the goal in a person's words.
	Statement string
	// GoalKey makes the goal STABLE across attempts -- the file id, for the
	// Library pass. Two analyses of one file are two runs of one goal.
	GoalKey string
	// RunKey makes this attempt distinct. Empty derives one from the clock.
	RunKey string
	// Input is the run's input envelope AND the goal's input. For the
	// Library pass it carries {fileId, artifactId, name}, which is what the
	// Training app keys its feed by.
	Input map[string]any
	// Steps are the template's step keys in order, with their kinds. Named
	// up front because the run's `stepOrder` is written at open: a feed that
	// learns the shape of the work as it happens cannot show progress.
	Steps []StepDecl
	// QueueSteps writes every declared step at `pending` at open, with its
	// seq, stepType, kind, call and dependsOn, so later stages exist as rows
	// before they run -- a pipeline's run page draws them as the stops ahead
	// (design record D13). The running intent Step writes later is a new
	// version of the same row. Off, nothing is written for a step until it
	// starts, which is how every pass before pipelines journaled.
	QueueSteps bool
	// RequestedVia is the surface the work arrived through.
	RequestedVia string
	// TriggeredBy is the run's triggeredBy. Empty is TriggeredBy(Template),
	// the journal's own driver-owned marker (see TriggerPrefix). A pipeline's run writes
	// "pipeline:<mode>" (pipelines.WorkTriggerPrefix), and that value is the
	// whole of what keeps integrations/work's dispatcher, sweep and re-run
	// acts off it: createWorkRun is the only writer of the field, so it is
	// written here, on the run's first version, or never.
	TriggeredBy string
	// GoalSignature, ParentRunID and Variables are what a CHILD run inherits
	// from the run that opened it (epic memql#5408, gap G2). A delegated app
	// session is recorded into a child run, and procedure learning mines the
	// recordings of ONE goal: a child that does not carry its parent's goal
	// signature belongs to no corpus, and one that does not carry the parent's
	// variables cannot say which goal input supplied each parameter. All three
	// are written at open, on the run's first version, so no reader ever sees
	// the run without them; blank ones are omitted like every other argument.
	GoalSignature string
	ParentRunID   string
	Variables     map[string]any
}

// StepDecl is one stage of the template -- for a pipeline, one step of its
// compiled plan.
type StepDecl struct {
	Key  string
	Kind string
	// StepType is what the step IS, the step concept's stepType. Empty is
	// "function", what every pass before pipelines wrote; a pipeline's step is
	// an "exec".
	StepType string
	// DependsOn are the keys of the steps this one waits for. Empty writes
	// nothing.
	DependsOn []string
	// Call names what the step invokes, by name only -- {construct, name,
	// stage} for a pipeline's step -- never a resolved argument, which may
	// carry a secret. Empty writes nothing.
	Call map[string]any
}

// defaultStepType is the stepType of a step that declares none.
const defaultStepType = "function"

func (d StepDecl) stepType() string { return firstNonEmpty(d.StepType, defaultStepType) }

func (d StepDecl) kind() string { return firstNonEmpty(d.Kind, KindDeterministic) }

// Run is one attempt, and the handle a caller holds.
type Run struct {
	j       *Journal
	goalID  string
	runID   string
	owner   string
	order   []StepDecl
	started time.Time
	// stop ends the heartbeat and done says it has ended; closing twice is
	// guarded by stopOnce, because a caller may close a run on more than one
	// path.
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// GoalID and RunID are what a caller records elsewhere -- a log line, a
// receipt. Both are "" for a nil run.
func (r *Run) GoalID() string {
	if r == nil {
		return ""
	}
	return r.goalID
}

func (r *Run) RunID() string {
	if r == nil {
		return ""
	}
	return r.runID
}

// Begin opens the goal and the run.
//
// The goal is written on EVERY begin rather than only the first. It is a
// read-merge insert at a stable id, so a second write is a new VERSION of one
// logical row rather than a duplicate -- the singleton pattern memql#4766
// settled for the cluster rows, and the reason it is right here too: the
// alternative is a read to find out whether to write, which races with the
// re-upload that made this the second attempt.
func (j *Journal) Begin(ctx context.Context, w Work) (*Run, error) {
	if j == nil || j.engine == nil {
		return nil, nil
	}
	owner := strings.TrimSpace(w.OwnerUserID)
	if owner == "" {
		// THE GATE. Everything below is downstream of it, which is the
		// precondition that makes this package's place on the internal-origin
		// allowlist a narrow one.
		return nil, fmt.Errorf("workjournal: ownerUserId is required -- a row written under a blank actor is readable by nobody")
	}
	template := strings.TrimSpace(w.Template)
	if template == "" {
		return nil, fmt.Errorf("workjournal: template is required")
	}

	started := j.now().UTC()
	runKey := strings.TrimSpace(w.RunKey)
	if runKey == "" {
		runKey = fmt.Sprintf("%d", started.UnixNano())
	}
	goalID, runID := workIDs(template, w.GoalKey, runKey)

	ctx = auth.ContextWithUserActor(ctx, owner)

	goalCall := call("mutation createWorkGoal",
		arg("goalId", goalID),
		arg("statement", firstNonEmpty(w.Statement, template)),
		arg("origin", "system"),
		arg("requestedVia", firstNonEmpty(w.RequestedVia, "api")),
		objectArg("input", w.Input),
	)
	if _, err := j.engine.Execute(auth.ContextWithInternalOrigin(ctx), goalCall); err != nil {
		return nil, fmt.Errorf("workjournal: open goal: %w", err)
	}

	order := make([]string, 0, len(w.Steps))
	for _, s := range w.Steps {
		if strings.TrimSpace(s.Key) != "" {
			order = append(order, s.Key)
		}
	}

	runCall := call("mutation createWorkRun",
		arg("runId", runID),
		arg("goalId", goalID),
		arg("automationName", template),
		arg("templateFingerprint", fingerprint(template, w.Steps)),
		objectArg("input", w.Input),
		// DRIVER-OWNED: this package writes and closes the run, and the
		// dispatcher must never adopt it. A runner that names its own trigger
		// (the pipelines driver's `pipeline:<mode>`) keeps it; every other
		// journal is marked with TriggerPrefix.
		arg("triggeredBy", firstNonEmpty(strings.TrimSpace(w.TriggeredBy), TriggeredBy(template))),
		arg("mode", "live"),
		arg("status", "running"),
		arg("nodeId", j.nodeID),
		arg("startedAt", started.Format(time.RFC3339)),
		arg("goalSignature", w.GoalSignature),
		arg("parentRunId", w.ParentRunID),
		objectArg("variables", w.Variables),
	)
	if _, err := j.engine.Execute(auth.ContextWithInternalOrigin(ctx), runCall); err != nil {
		return nil, fmt.Errorf("workjournal: open run: %w", err)
	}
	// The first heartbeat rides this write: `createWorkRun` takes no
	// heartbeatAt, and a run whose driver dies before the first tick is then
	// judged from the moment it opened rather than from its start time alone.
	opened := []string{arg("runId", runID), arg("heartbeatAt", started.Format(time.RFC3339Nano))}
	if len(order) > 0 {
		opened = append(opened, stringListArg("stepOrder", order))
	}
	j.exec(ctx, call("mutation updateWorkRun", opened...))
	run := &Run{
		j: j, goalID: goalID, runID: runID, owner: owner, order: w.Steps, started: started,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	if w.QueueSteps {
		run.queue(ctx)
	}
	// The goal is now being worked. `createWorkGoal` stamps `open` and has
	// no status argument, so this is the only thing that can say so.
	j.exec(ctx, call("mutation updateWorkGoal",
		arg("goalId", goalID),
		arg("status", "active"),
	))
	go run.heartbeat(ctx)
	return run, nil
}

// heartbeat says the run is still being worked, every HeartbeatInterval,
// until the run closes or the caller's context ends.
//
// A GOROUTINE AND NOT A WRITE PER STEP, because the stages that need it most
// are the long ones: the Library pass embeds every chunk of a large file inside
// one `index` step, and a delegated app session is one step that can run for
// hours. A beat written only at step boundaries would let the work sweep close
// either as abandoned while it was still running.
//
// The caller's context ending stops it as well as close() does: the pass runs
// on that context, so a pass whose context is gone is not being worked any
// more, and a heartbeat that outlived it would keep a dead run looking alive.
func (r *Run) heartbeat(ctx context.Context) {
	defer close(r.done)
	every := r.j.beat
	if every <= 0 {
		every = HeartbeatInterval
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ctx.Done():
			return
		case <-tick.C:
			r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation updateWorkRun",
				arg("runId", r.runID),
				arg("heartbeatAt", r.j.now().UTC().Format(time.RFC3339Nano)),
			))
		}
	}
}

// stopHeartbeat ends the heartbeat and waits for it, so no beat can land after
// the close that follows. Safe to call more than once.
func (r *Run) stopHeartbeat() {
	if r == nil || r.stop == nil {
		return
	}
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// IDs is the goal id and the run id Begin derives for w, with nothing written.
// A runner that must NAME its work before opening it -- the pipelines driver
// records them on its own row first, so a replica that stops between the two
// can never leave a work run that nothing names, and that no sweep would ever
// close -- asks here, and Begin then opens exactly these. An empty RunKey is
// refused: Begin derives one from the clock, which no caller can predict.
func IDs(w Work) (goalID, runID string, err error) {
	template := strings.TrimSpace(w.Template)
	runKey := strings.TrimSpace(w.RunKey)
	if template == "" || runKey == "" {
		return "", "", fmt.Errorf("workjournal: a template and a run key are what the ids are derived from")
	}
	goalID, runID = workIDs(template, w.GoalKey, runKey)
	return goalID, runID, nil
}

// workIDs is the one derivation of a goal's and a run's ids, shared by Begin
// and IDs so the two cannot drift.
func workIDs(template, goalKey, runKey string) (goalID, runID string) {
	return deriveID("goal", template, goalKey), deriveID("run", template, goalKey+"|"+runKey)
}

// queue writes every declared step at `pending` (Work.QueueSteps): the same
// row id, seq, type and kind the running intent writes later, so that intent
// is a new version of this row rather than a second row beside it. A pending
// step has not started, so it carries no startedAt. A key declared twice is
// queued once: both declarations name one row.
func (r *Run) queue(ctx context.Context) {
	ctx = auth.ContextWithUserActor(ctx, r.owner)
	queued := map[string]bool{}
	for _, s := range r.order {
		if strings.TrimSpace(s.Key) == "" || queued[s.Key] {
			continue
		}
		queued[s.Key] = true
		seq, decl := r.decl(s.Key)
		r.j.exec(ctx, call("mutation createWorkStep",
			arg("stepId", deriveID("step", r.runID, s.Key)),
			arg("runId", r.runID),
			arg("key", s.Key),
			intArg("seq", seq),
			arg("stepType", decl.stepType()),
			arg("kind", decl.kind()),
			objectArg("call", decl.Call),
			stringListArg("dependsOn", decl.DependsOn),
			arg("status", "pending"),
			intArg("attempt", 1),
			arg("idempotencyKey", r.runID+":"+s.Key+":1"),
		))
	}
}

// Reopen returns a handle on a run this journal -- or another replica's --
// opened, for a driver that resumes it: the pipelines driver that takes over a
// run whose first driver went silent. It WRITES NOTHING. The goal, the run and
// its queued steps already exist, and a resumed driver that wrote at reopen
// would be a second opening of one run.
//
// The ids may be bare, as Begin returns them, or canonical, as a stored
// reference reads back ("v1:work:run:<id>"): a relationship field is stored
// canonicalized, and the pipelines run row is where a resumed driver reads
// them. Both name the same rows, because a step's id is derived from the
// BARE run id -- derived from the canonical one it would be a different row,
// and the resumed step would appear beside its own pending row.
//
// THE GATE BEGIN KEEPS IS KEPT HERE. Reopen is the second way to a handle, and
// the allowlist entry this package rests on is about every call site: with no
// owner there is no handle, and a row is never written under a blank actor.
// With no goal or no run there is nothing to address. Each answers nil, which
// every method tolerates, and the refusal is logged rather than returned --
// the run's work goes on without a journal, as it would with none wired.
//
// started is when the run STARTED, read off its row, not when this replica
// picked it up: the close reports the run's wall clock, which a takeover does
// not reset. A zero started omits the wall clock rather than measuring from
// year one.
func (j *Journal) Reopen(ownerUserID, goalID, runID string, steps []StepDecl, started time.Time) *Run {
	if j == nil || j.engine == nil {
		return nil
	}
	owner := strings.TrimSpace(ownerUserID)
	goalID = bareID(goalConcept, goalID)
	runID = bareID(runConcept, runID)
	if owner == "" || goalID == "" || runID == "" {
		j.logger.Warn("workjournal: refused to reopen a run without its owner, goal and run ids; nothing will be journaled for it",
			"run", runID, "goal", goalID, "ownerPresent", owner != "")
		return nil
	}
	if !started.IsZero() {
		started = started.UTC()
	}
	return &Run{j: j, goalID: goalID, runID: runID, owner: owner, order: steps, started: started}
}

// The concepts whose ids this journal derives, for bareID.
const (
	goalConcept = "v1:work:goal"
	runConcept  = "v1:work:run"
)

// bareID is id with its concept prefix removed, or id itself when it is
// already bare. Only the named concept's prefix is removed: these are ids this
// journal derived, hex with no colon of their own, and anything else is left
// as given rather than cut at a guess.
func bareID(concept, id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), concept+":")
}

// Step is one stage in flight.
type Step struct {
	run     *Run
	id      string
	key     string
	started time.Time
}

// Step writes the `running` version -- the INTENT, before the body executes.
// The receipt is a second version of the same row (spec section D), which is
// why a step that never reports back is visibly a step that started and did
// not finish rather than a step that never happened.
//
// The step's type, call and dependencies come from its declaration, so the
// intent of a queued step (Work.QueueSteps) restates what its pending version
// said rather than overwriting it -- the insert is a read-merge, and a
// "function" written over an "exec" would be a lie on every version after.
// A declaration naming none of the three writes the intent exactly as every
// pass before pipelines did.
func (r *Run) Step(ctx context.Context, key string) *Step {
	if r == nil || r.j == nil {
		return nil
	}
	seq, decl := r.decl(key)
	started := r.j.now().UTC()
	stepID := deriveID("step", r.runID, key)
	r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation createWorkStep",
		arg("stepId", stepID),
		arg("runId", r.runID),
		arg("key", key),
		intArg("seq", seq),
		arg("stepType", decl.stepType()),
		arg("kind", decl.kind()),
		objectArg("call", decl.Call),
		stringListArg("dependsOn", decl.DependsOn),
		arg("status", "running"),
		intArg("attempt", 1),
		arg("idempotencyKey", r.runID+":"+key+":1"),
		arg("startedAt", started.Format(time.RFC3339)),
	))
	return &Step{run: r, id: stepID, key: key, started: started}
}

// Done writes the receipt.
func (s *Step) Done(ctx context.Context, result map[string]any) {
	s.finish(ctx, Receipt{Status: stepDone, Result: result})
}

// Failed writes the receipt for a stage that did not finish.
func (s *Step) Failed(ctx context.Context, code, message string) {
	s.finish(ctx, Receipt{Status: stepFailed, Code: code, Message: message})
}

// Skipped records a stage the template declares and this run did not need.
// It is written rather than omitted so the run's steps still add up to its
// declared order -- a missing row and a skipped one look identical to a
// reader, and only one of them is true.
func (s *Step) Skipped(ctx context.Context, why string) {
	s.finish(ctx, Receipt{Status: stepSkipped, Result: map[string]any{"reason": why}})
}

// Cancelled records a step stopped before it finished: its run was cancelled
// by a person, or superseded by a newer push to the same pull request (design
// record D11). Written for the reason Skipped is: a step the run declared and
// never closed reads as one still running.
func (s *Step) Cancelled(ctx context.Context, why string) {
	s.finish(ctx, Receipt{Status: stepCancelled, Result: map[string]any{"reason": why}})
}

// The step concept's terminal statuses: how a step ENDED, which is all a
// receipt may say.
const (
	stepDone      = "done"
	stepFailed    = "failed"
	stepSkipped   = "skipped"
	stepCancelled = "cancelled"
)

// Receipt is a step's close with everything a runner reports about it: a
// pipeline step's executor says where the step ran, how long its command took
// and which Library files hold its log and its artifacts (design record D11).
// Every empty field is left unwritten, so a receipt carrying only a status is
// the receipt Done, Failed and Skipped have always written.
type Receipt struct {
	// Status is how the step ended: done, failed, skipped or cancelled. Any
	// other value is refused and logged, and nothing is written.
	Status string
	// Result is the trimmed result, written as the step's result.
	Result map[string]any
	// Code and Message are the failure: a catalogued code and its words,
	// written as errorCode and errorMessage.
	Code, Message string
	// DurationMs is the body's duration as the RUNNER measured it -- it saw
	// the command run, where the journal sees a round trip. Zero means it
	// reported none, and the time since the running intent is written.
	DurationMs int64
	// Binding is where the step ran: surface, nodeId, workerId,
	// machineLabels, jobName.
	Binding map[string]any
	// LogFileID is the Library file holding the step's full log.
	LogFileID string
	// ArtifactFileIDs are the Library files the step's declared artifacts
	// became.
	ArtifactFileIDs []string
}

// Finish writes a step's receipt with everything its runner reported.
//
// logFileId and artifactFileIds are updateWorkStep arguments the pipelines
// epic adds (dsl/work, epic memql#5477); each is written only when the
// receipt names one, so a receipt without them is unchanged by the epic.
func (s *Step) Finish(ctx context.Context, r Receipt) {
	if s == nil || s.run == nil || s.run.j == nil {
		return
	}
	switch r.Status {
	case stepDone, stepFailed, stepSkipped, stepCancelled:
	default:
		s.run.j.logger.Warn("workjournal: a step receipt named a status that is not one a step ends in; nothing was written",
			"status", r.Status, "step", s.key, "run", s.run.runID)
		return
	}
	s.finish(ctx, r)
}

func (s *Step) finish(ctx context.Context, r Receipt) {
	if s == nil || s.run == nil || s.run.j == nil {
		return
	}
	finished := s.run.j.now().UTC()
	durationMs := r.DurationMs
	if durationMs <= 0 {
		durationMs = finished.Sub(s.started).Milliseconds()
	}
	s.run.j.exec(auth.ContextWithUserActor(ctx, s.run.owner), call("mutation updateWorkStep",
		arg("stepId", s.id),
		arg("status", r.Status),
		objectArg("result", r.Result),
		arg("errorCode", r.Code),
		arg("errorMessage", r.Message),
		arg("finishedAt", finished.Format(time.RFC3339)),
		intArg("durationMs", durationMs),
		objectArg("binding", r.Binding),
		arg("logFileId", r.LogFileID),
		stringListArg("artifactFileIds", r.ArtifactFileIDs),
	))
}

// Heartbeat says the run is still being driven: heartbeatAt, and the node
// beating, which is the node running it -- after a takeover, the replica that
// reopened it rather than the one that opened it. It writes NOTHING ELSE. The
// beat comes from a goroutine that can lose a race with the run's close, and
// a beat that also wrote a status would reopen a closed run.
func (r *Run) Heartbeat(ctx context.Context) {
	if r == nil || r.j == nil {
		return
	}
	r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation updateWorkRun",
		arg("runId", r.runID),
		arg("heartbeatAt", r.j.now().UTC().Format(time.RFC3339)),
		arg("nodeId", r.j.nodeID),
	))
}

// Succeeded closes the run.
func (r *Run) Succeeded(ctx context.Context, outcome map[string]any) {
	r.close(ctx, "succeeded", outcome, "", "")
}

// Failed closes the run with the reason.
func (r *Run) Failed(ctx context.Context, code, message string) {
	r.close(ctx, "failed", nil, code, message)
}

// Cancelled closes a run stopped before it finished -- by a person, or by a
// newer push to the same pull request (design record D11) -- with the reason.
// Its goal closes with it, as with every other close.
func (r *Run) Cancelled(ctx context.Context, code, message string) {
	r.close(ctx, "cancelled", nil, code, message)
}

func (r *Run) close(ctx context.Context, status string, outcome map[string]any, code, message string) {
	if r == nil || r.j == nil {
		return
	}
	// Before the close is written, so no beat can land after it: a heartbeat
	// on a finished run is harmless to the sweep, but it is a lie in the
	// run's history.
	r.stopHeartbeat()
	finished := r.j.now().UTC()
	errorCode, errorMessage := arg("errorCode", code), arg("errorMessage", message)
	if status == "succeeded" {
		// THE ONE BLANK THIS PACKAGE SENDS, and it is sent on purpose. A run
		// that succeeded carries no error, and the read-merge keeps whatever an
		// earlier write named -- which is how a Library pass that finished
		// kept the automation_not_runnable a dispatcher wrote over it while it
		// ran. Clearing is the only way to say "none" on an update.
		errorCode, errorMessage = clearArg("errorCode"), clearArg("errorMessage")
	}
	// A reopened run handed no start cannot say how long it ran, so it says
	// nothing rather than a wall clock measured from year one.
	var spent map[string]any
	if !r.started.IsZero() {
		spent = map[string]any{"wallClockMs": finished.Sub(r.started).Milliseconds()}
	}
	r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation updateWorkRun",
		arg("runId", r.runID),
		arg("status", status),
		objectArg("outcome", outcome),
		errorCode,
		errorMessage,
		arg("finishedAt", finished.Format(time.RFC3339)),
		objectArg("spent", spent),
	))
	// The goal closes with its run. A goal whose only run is over is not
	// still "active", and leaving it that way would make every finished
	// analysis read as work in progress.
	reason := "the run finished"
	if status == "cancelled" {
		reason = "the run was cancelled"
	}
	r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation updateWorkGoal",
		arg("goalId", r.goalID),
		arg("status", "closed"),
		arg("closedAt", finished.Format(time.RFC3339)),
		arg("closeReason", firstNonEmpty(message, reason)),
	))
}

// decl answers a step's position in the declared order and its declaration.
// A key nobody declared sits after every declared one, with the defaults.
func (r *Run) decl(key string) (int, StepDecl) {
	for i, s := range r.order {
		if s.Key == key {
			return i, s
		}
	}
	return len(r.order), StepDecl{Key: key}
}

// exec runs a write and LOGS a failure rather than returning it.
//
// That is deliberate and it is the one judgment in this package worth
// arguing with. The journal is a RECORD of work, not the work: a pass that
// extracted, chunked and embedded a file successfully must not be reported
// as failed because a step row did not land. So a write that fails is loud
// in the log and invisible to the caller -- except at Begin, where a failure
// means there is no run at all and the caller gets it.
func (j *Journal) exec(ctx context.Context, q string) {
	if j == nil || j.engine == nil {
		return
	}
	if _, err := j.engine.Execute(auth.ContextWithInternalOrigin(ctx), q); err != nil {
		j.logger.Warn("workjournal: a journal write did not land", "error", err, "call", firstWord(q))
	}
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// call assembles `mutation name(a: 1, b: 2)`, dropping empty arguments.
//
// An empty argument is DROPPED rather than sent blank because every mutation
// here is a read-merge update: sending `errorMessage: ""` on a success would
// be a write, and on the update path it would clear a value a previous
// version legitimately holds.
func call(head string, args ...string) string {
	kept := make([]string, 0, len(args))
	for _, a := range args {
		if a != "" {
			kept = append(kept, a)
		}
	}
	return head + "(" + strings.Join(kept, ", ") + ")"
}

// arg renders a string argument, or "" when the value is blank.
//
// QuoteString, never %q: the two diverge on four control characters, and a
// summary or an error message is exactly the kind of value that carries one.
func arg(name, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return name + ": " + langparser.QuoteString(value)
}

// clearArg renders an argument as the empty string -- a deliberate CLEAR on a
// read-merge update, which arg would drop. See close for the one caller.
func clearArg(name string) string {
	return name + `: ""`
}

func intArg[T int | int64](name string, value T) string {
	return fmt.Sprintf("%s: %d", name, value)
}

// objectArg renders an object argument as a JSON literal. The DSL's object
// literal and JSON agree on the shapes that appear here -- string keys,
// scalar and nested values -- and going through encoding/json is what keeps
// a value containing a brace or a quote from ending the literal early.
func objectArg(name string, value map[string]any) string {
	if len(value) == 0 {
		return ""
	}
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return name + ": " + string(b)
}

func stringListArg(name string, values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, langparser.QuoteString(v))
	}
	return name + ": [" + strings.Join(quoted, ", ") + "]"
}

// deriveID makes a stable BARE short id. Bare because that is what a
// mutation's id argument takes -- the engine canonicalizes on the way in --
// and a hash because the inputs are file ids and template names that are
// themselves canonical ids full of colons.
func deriveID(kind, scope, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + scope + "\x00" + key))
	return hex.EncodeToString(sum[:16])
}

// fingerprint is what changes when the template changes. It covers the step
// KEYS and KINDS, so re-ordering the stages or making a deterministic stage
// reasoning is visible on every run written afterwards.
func fingerprint(template string, steps []StepDecl) string {
	h := sha256.New()
	h.Write([]byte(template))
	for _, s := range steps {
		h.Write([]byte("\x00" + s.Key + "\x00" + s.Kind))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstWord(s string) string {
	if i := strings.Index(s, "("); i > 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// Bindings
// ---------------------------------------------------------------------------

// StampBinding records on a step what the dispatch decided (spec section C).
//
// A SECOND WRITER OF updateWorkStep, and it is deliberately not the Step
// handle above. A binding is made by whatever DISPATCHED the step -- for a
// script step that is `runScript`, which runs inside the tool loop and holds
// no journal handle -- so it is addressed by step id rather than by a handle
// somebody would have to thread through the dispatcher.
//
// It is here rather than at the dispatcher for the reason everything else in
// this package is: `updateWorkStep` is `@serverOnly`, the internal-origin
// stamp is allowlisted per package, and `integrations/skills` is not a
// package that list should admit.
//
// AN EMPTY BINDING IS NOT WRITTEN. `objectArg` drops an empty map, so a
// dispatch that decided nothing leaves the field ABSENT rather than writing
// `{}` -- and absent is the honest reading of "no dispatch has happened yet".
// The distinction matters because `null` is a different thing again: it fails
// the concept's `object` type and would refuse the whole row, which is why
// nothing here ever renders one.
func (j *Journal) StampBinding(ctx context.Context, ownerUserID, stepID string, binding map[string]any) error {
	if j == nil || j.engine == nil {
		return nil
	}
	stepID = strings.TrimSpace(stepID)
	if stepID == "" || len(binding) == 0 {
		return nil
	}
	owner := strings.TrimSpace(ownerUserID)
	if owner == "" {
		return fmt.Errorf("workjournal: a binding needs the step owner's id to be written under")
	}
	_, err := j.engine.Execute(
		auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)),
		call("mutation updateWorkStep", arg("stepId", stepID), objectArg("binding", binding)),
	)
	return err
}
