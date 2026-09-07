package agent

// toolrecord.go -- what an agent's tool calls are recorded AS, now that they
// are not `v1:planner:task` rows (memql#5050).
//
// # What this replaces
//
// component/memql/taskstamp wrapped every tool dispatch and wrote a
// `v1:planner:task` row with `category='toolInvocation'`. When a turn had no
// Plan to parent one to -- a chat-driven turn, which was most of them -- it
// MINTED a synthetic `kind='adHocAction'` Plan first, so the invariant "every
// Task has a parent Plan" held.
//
// Every one of its five failure paths logged and continued. That was the right
// call for a bookkeeping side-record, and it is also why the package could not
// simply be left pointing at a retired concept: with the concepts gone it
// would emit two or three warnings per tool call, on every agent tool call in
// the product, and nothing would fail.
//
// # An observation, not a step
//
// The obvious reading of "tool invocations become work steps" is a
// `v1:work:step` row per call. That is the wrong row, and the concept says so:
// a step has a `key` "from the template (the automation step id)" and a `seq`
// that is its "position in the run's step order". A tool call an LLM decides
// to make in the middle of a turn has neither -- it is not a node of the
// template, it is something that happened INSIDE one. Writing one anyway would
// put rows in `stepOrder`'s domain that no template names, and resume walks
// that order.
//
// `v1:work:observation` is the row the spine already has for this, and its
// `kind` enum leads with `tool_result`. It carries `runId`, the `stepKey` that
// produced it, structured `data`, and a `content` string that is "the
// embedding source, so observations are recall-able" -- which is strictly more
// than the task row gave, since a `toolInvocation` task was never searchable.
//
// # No run means no record, and that is the deliberate part
//
// taskstamp minted a Plan when there was no plan. The equivalent here would be
// minting a run when there is no run, and it is refused: `v1:work:run` carries
// `automationName string!`, so a synthetic run has to name a template that
// does not exist, and it would then be picked up by the run dispatcher
// (memql#5054), claimed, and executed. The ad-hoc Plan was inert; an ad-hoc
// run would not be.
//
// So a bare agent turn -- one nobody's goal asked for -- records nothing. The
// consumer that needed the ad-hoc records was the authoring transcript, which
// reads them per RUN now.

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/znasllc-io/memql/core/common"
)

// errNoToolExecutor is what a recorder with nothing to dispatch to answers.
// Named rather than inline so a caller can tell it from a tool's own failure.
var errNoToolExecutor = errors.New("agent: no tool executor wired")

// ToolInvocationRecorder is the seam the work integration satisfies. Declared
// here rather than imported so integrations/agent does not depend on
// integrations/work: the write lands on an @serverOnly construct, and the
// internal-origin stamp is allowlisted per PACKAGE to integrations/work.
type ToolInvocationRecorder interface {
	// RecordToolInvocation writes one tool_result observation against a run.
	// Called on the hot path of every tool call, so it must not block on
	// anything slow, and its failures are the caller's to swallow.
	RecordToolInvocation(ctx context.Context, rec ToolInvocation) error
}

// ToolInvocation is one recorded call.
type ToolInvocation struct {
	RunId       string
	StepKey     string
	OwnerUserId string
	// Seq orders the calls within a run. Observations have no seq column, so
	// it rides in `data` -- which is what the transcript reader sorts on.
	// Row timestamps are NOT good enough here: two calls in the same turn can
	// share a createdAt, and a transcript whose order is unstable reproduces
	// a different automation each time it is captured.
	Seq      int
	ToolName string
	Args     map[string]any
	IsError  bool
	// Error is the failure text when IsError. The RESULT is deliberately not
	// recorded: a tool result can be arbitrarily large and can carry anything
	// the tool read, and the transcript only needs the call.
	Error string
}

// toolExecutor is the dispatch this wraps -- the engine's own
// ExecuteToolByName.
type toolExecutor interface {
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
}

// toolRecorder wraps tool dispatch and records each call against the run in
// context, when there is one.
//
// It keeps taskstamp's wrapper SHAPE on purpose: one place that sees every
// tool call, so the recording cannot be forgotten at a new call site. Only
// what it writes has changed.
type toolRecorder struct {
	exec     toolExecutor
	recorder ToolInvocationRecorder
	logger   *slog.Logger

	// seq counts calls per run within this process. A run executes on ONE
	// replica (the dispatcher claims it), so a per-process counter is the
	// whole ordering -- there is no second writer to race with.
	mu   sync.Mutex
	seqs map[string]int
}

func newToolRecorder(exec toolExecutor, logger *slog.Logger) *toolRecorder {
	return &toolRecorder{exec: exec, logger: logger, seqs: map[string]int{}}
}

// SetRecorder installs the work-spine writer. Nil leaves dispatch working and
// records nothing, which is the correct state for a node with no work
// integration -- and unlike taskstamp's nil paths, it is not a failure being
// swallowed.
func (r *toolRecorder) SetRecorder(rec ToolInvocationRecorder) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorder = rec
}

func (r *toolRecorder) recorderRef() ToolInvocationRecorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recorder
}

// nextSeq returns the next per-run call index.
func (r *toolRecorder) nextSeq(runId string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.seqs[runId]
	r.seqs[runId] = n + 1
	return n
}

// ExecuteToolByName dispatches the tool and records the call.
//
// THE TOOL RUNS FIRST AND ITS RESULT IS RETURNED UNCHANGED, whatever the
// recording does. That ordering is taskstamp's one genuinely good property and
// it is kept: a bookkeeping row must never be able to fail a tool call, and
// recording after the fact means the record describes what actually happened
// rather than what was about to be attempted.
func (r *toolRecorder) ExecuteToolByName(ctx context.Context, toolName string, args map[string]any) (string, error) {
	if r == nil || r.exec == nil {
		return "", errNoToolExecutor
	}
	out, execErr := r.exec.ExecuteToolByName(ctx, toolName, args)

	rec := r.recorderRef()
	if rec == nil {
		return out, execErr
	}
	run, ok := common.RunFromContext(ctx)
	if !ok {
		// No run: nothing this call belongs to. See the header.
		return out, execErr
	}

	inv := ToolInvocation{
		RunId:       run.RunId,
		StepKey:     run.StepKey,
		OwnerUserId: run.OwnerUserId,
		Seq:         r.nextSeq(run.RunId),
		ToolName:    toolName,
		Args:        args,
		IsError:     execErr != nil,
	}
	if execErr != nil {
		inv.Error = execErr.Error()
	}
	if err := rec.RecordToolInvocation(ctx, inv); err != nil && r.logger != nil {
		// Logged and swallowed, like taskstamp -- for the same reason, which
		// is still a good one: a record of a tool call must not fail the tool
		// call. The difference from taskstamp is that this is now ONE site
		// rather than five, and it can no longer fire on every call in the
		// product, because it only runs inside a run.
		r.logger.Warn("agent: recording a tool invocation failed; the call itself succeeded",
			"component", ComponentName, "tool", toolName, "run", run.RunId, "error", err)
	}
	return out, execErr
}

// SetToolInvocationRecorder installs the work-spine writer on this replier's
// tool path.
//
// Called from app wiring on the agent node, where integrations/work is in
// scope. A replier with none dispatches tools exactly as before and records
// nothing -- which is the honest state on a node with no work integration,
// and is why this is a setter rather than a constructor argument: the replier
// is built before the plug-in set is resolved.
func (r *Replier) SetToolInvocationRecorder(rec ToolInvocationRecorder) {
	if r == nil || r.stamper == nil {
		return
	}
	r.stamper.SetRecorder(rec)
}
