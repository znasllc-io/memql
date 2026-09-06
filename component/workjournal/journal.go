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

// Journal writes the rows.
type Journal struct {
	engine Executor
	logger *slog.Logger
	now    func() time.Time
	nodeID string
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
	return &Journal{engine: engine, logger: logger, now: time.Now, nodeID: nodeID}
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
	// RequestedVia is the surface the work arrived through.
	RequestedVia string
}

// StepDecl is one stage of the template.
type StepDecl struct {
	Key  string
	Kind string
}

// Run is one attempt, and the handle a caller holds.
type Run struct {
	j       *Journal
	goalID  string
	runID   string
	owner   string
	order   []StepDecl
	started time.Time
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
	goalID := deriveID("goal", template, w.GoalKey)
	runKey := strings.TrimSpace(w.RunKey)
	if runKey == "" {
		runKey = fmt.Sprintf("%d", started.UnixNano())
	}
	runID := deriveID("run", template, w.GoalKey+"|"+runKey)

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
		arg("triggeredBy", "system"),
		arg("mode", "live"),
		arg("status", "running"),
		arg("nodeId", j.nodeID),
		arg("startedAt", started.Format(time.RFC3339)),
	)
	if _, err := j.engine.Execute(auth.ContextWithInternalOrigin(ctx), runCall); err != nil {
		return nil, fmt.Errorf("workjournal: open run: %w", err)
	}
	if len(order) > 0 {
		j.exec(ctx, call("mutation updateWorkRun",
			arg("runId", runID),
			stringListArg("stepOrder", order),
		))
	}
	// The goal is now being worked. `createWorkGoal` stamps `open` and has
	// no status argument, so this is the only thing that can say so.
	j.exec(ctx, call("mutation updateWorkGoal",
		arg("goalId", goalID),
		arg("status", "active"),
	))
	return &Run{j: j, goalID: goalID, runID: runID, owner: owner, order: w.Steps, started: started}, nil
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
func (r *Run) Step(ctx context.Context, key string) *Step {
	if r == nil || r.j == nil {
		return nil
	}
	seq, kind := r.position(key)
	started := r.j.now().UTC()
	stepID := deriveID("step", r.runID, key)
	r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation createWorkStep",
		arg("stepId", stepID),
		arg("runId", r.runID),
		arg("key", key),
		intArg("seq", seq),
		arg("stepType", "function"),
		arg("kind", kind),
		arg("status", "running"),
		intArg("attempt", 1),
		arg("idempotencyKey", r.runID+":"+key+":1"),
		arg("startedAt", started.Format(time.RFC3339)),
	))
	return &Step{run: r, id: stepID, key: key, started: started}
}

// Done writes the receipt.
func (s *Step) Done(ctx context.Context, result map[string]any) {
	s.finish(ctx, "done", result, "", "")
}

// Failed writes the receipt for a stage that did not finish.
func (s *Step) Failed(ctx context.Context, code, message string) {
	s.finish(ctx, "failed", nil, code, message)
}

// Skipped records a stage the template declares and this run did not need.
// It is written rather than omitted so the run's steps still add up to its
// declared order -- a missing row and a skipped one look identical to a
// reader, and only one of them is true.
func (s *Step) Skipped(ctx context.Context, why string) {
	s.finish(ctx, "skipped", map[string]any{"reason": why}, "", "")
}

func (s *Step) finish(ctx context.Context, status string, result map[string]any, code, message string) {
	if s == nil || s.run == nil || s.run.j == nil {
		return
	}
	finished := s.run.j.now().UTC()
	s.run.j.exec(auth.ContextWithUserActor(ctx, s.run.owner), call("mutation updateWorkStep",
		arg("stepId", s.id),
		arg("status", status),
		objectArg("result", result),
		arg("errorCode", code),
		arg("errorMessage", message),
		arg("finishedAt", finished.Format(time.RFC3339)),
		intArg("durationMs", int(finished.Sub(s.started).Milliseconds())),
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

func (r *Run) close(ctx context.Context, status string, outcome map[string]any, code, message string) {
	if r == nil || r.j == nil {
		return
	}
	finished := r.j.now().UTC()
	r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation updateWorkRun",
		arg("runId", r.runID),
		arg("status", status),
		objectArg("outcome", outcome),
		arg("errorCode", code),
		arg("errorMessage", message),
		arg("finishedAt", finished.Format(time.RFC3339)),
		objectArg("spent", map[string]any{"wallClockMs": finished.Sub(r.started).Milliseconds()}),
	))
	// The goal closes with its run. A goal whose only run is over is not
	// still "active", and leaving it that way would make every finished
	// analysis read as work in progress.
	r.j.exec(auth.ContextWithUserActor(ctx, r.owner), call("mutation updateWorkGoal",
		arg("goalId", r.goalID),
		arg("status", "closed"),
		arg("closedAt", finished.Format(time.RFC3339)),
		arg("closeReason", firstNonEmpty(message, "the run finished")),
	))
}

func (r *Run) position(key string) (int, string) {
	for i, s := range r.order {
		if s.Key == key {
			return i, firstNonEmpty(s.Kind, KindDeterministic)
		}
	}
	return len(r.order), KindDeterministic
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

func intArg(name string, value int) string {
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
