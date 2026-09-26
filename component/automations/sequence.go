package automations

// sequence.go -- a statement body, run in order (epic memql#5370, task
// memql#5372; D12 and D14 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// runSequence is the loop a statement body runs through: the body's own list,
// every iteration of a `for`, every branch of a `parallel` and every logic
// called as a statement. The compiler emitted the steps in source order and
// checked every name, so the loop does what the source says in the order it
// says it:
//
//   - a step whose condition is false is skipped and binds nothing, so a
//     later read of its name is absent;
//   - a step that succeeds binds its value (statementValue) under its `binds`
//     name in the current frame;
//   - `retry(n)` retries a failed call up to n more times, whatever its
//     `on error`; `on error continue` then records the failure, leaves the name
//     absent and goes on; without it the failure ends the sequence;
//   - a return step, or a call step marked `returns`, ends the sequence with its
//     value -- inside a `for` it ends the body the loop is in, not only the
//     iteration.
//
// The top-level list of an automation also writes the journal, honours both
// cancellations and advances the chain head, exactly as the legacy loop in
// executeWithEvent does for every other automation; a nested list does none of
// that, as a forEach's children never did. A logic's statements journal too
// (logic_statements.go): as rows of the run they were called in, or as a run
// of their own that opens at the logic's first write. Every step is keyed in
// its run by its list's path and its id (stepKeyIn), which is what a logic it
// calls journals under.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// seqOutcome is how a sequence ended: Returned, with Value, when a return
// ended it; neither when it ran off its end.
type seqOutcome struct {
	Returned bool
	Value    any
}

// sequenceRun is what running one list of steps needs beyond the list.
type sequenceRun struct {
	stepCtx *StepContext
	// top marks an automation's own list: it records its steps on the run,
	// polls for cancellation and advances the chain head.
	top bool
	// orders marks a list whose steps are its run's step order: an
	// automation's own list (top) and a directly called logic's statements.
	orders bool
	// journal, when set, writes this list's steps: an automation's own list
	// against its run, a directly called logic's against the run it opens at
	// its first write, and a logic's statements inside a caller's run
	// (rowsOnly: step rows only, the run row being the caller's).
	journal    *workJournal
	rowsOnly   bool
	chainHead  string
	cancelPoll *cancelPoller
	// resumed is a resumed run's own list's view of its journal (ResumeFrom).
	resumed *resumedList
}

// resumedList is what a resumed statement body knows from its journal. The
// statements before the one it resumes at that finished bind the values they
// recorded and do not run; those that failed under `on error continue` stay
// absent, as they were. The statement it resumes at runs on its next attempt,
// and every statement after it runs as it would have.
type resumedList struct {
	done      map[string]*MinimalStepResult
	continued map[string]bool
	at        string
	// attempt is the attempt `at` last recorded.
	attempt int
}

// attemptBase is how many attempts of step the journal already holds: its
// attempts go on from there.
func (r *sequenceRun) attemptBase(step *Step) int {
	if r.resumed != nil && step.ID == r.resumed.at {
		return max(r.resumed.attempt, 1)
	}
	return 0
}

// maxJournaledRows bounds the rows a query statement's recorded value holds,
// as shouldIncludeResult bounds a bundle's; a longer read is not recorded, and
// a resumed body reads it again.
const maxJournaledRows = 100

// journaledValue is the value the journal records for a statement that binds
// or returns one (StepResult.Bound), as its consumers read it.
func journaledValue(step *Step, value any) any {
	if step.Binds == "" && !step.Returns {
		return nil
	}
	v := unwrapStatementValue(value)
	if rows, ok := v.([]any); ok && len(rows) > maxJournaledRows && isQueryStatement(step) {
		return nil
	}
	return v
}

// isQueryStatement reports whether a statement is a query call: a read, which
// a resumed body may run again.
func isQueryStatement(step *Step) bool {
	return step.Type == StepTypeFunction && step.Function != nil && strings.EqualFold(step.Function.Kind, "query")
}

// bodyRunnerKey carries the executor's sequence runner to the step executors
// that hold lists of their own (a `for`, a parallel branch), which live in the
// steps package and so reach it through the context.
type bodyRunnerKey struct{}

type bodyRunner func(ctx context.Context, steps []*Step, ev *Evaluator) (seqOutcome, error)

// RunStatementBody runs a nested statement list -- a loop iteration's body, a
// parallel branch -- through the sequence runner of the automation it belongs
// to, over ev (a child frame the caller opened). key names the list within
// the step that holds it: `<step id>/<n>` for a `for`'s nth item, the block's
// id for a branch. It returns whether a return ended the list and with what
// value.
func RunStatementBody(ctx context.Context, key string, steps []*Step, stepCtx *StepContext, ev *Evaluator) (bool, any, error) {
	run, ok := ctx.Value(bodyRunnerKey{}).(bodyRunner)
	if !ok || run == nil {
		return false, nil, fmt.Errorf("a statement body's nested steps ran outside a statement run")
	}
	out, err := run(withListKey(ctx, joinStepKey(listKeyFrom(ctx), key)), steps, ev)
	return out.Returned, out.Value, err
}

// listKeyKey carries the key path of the statement list a step runs in:
// empty for a run's own list, the path of its holder for a nested one. A
// step's key in its run is that path and its id (stepKeyIn): the key a model
// call it makes is journaled at, and the key a logic it calls journals its
// statements under. Ids are unique within one list, so a nested step's key
// needs its holder's path to be unique within the run, and a `for`'s the item
// too.
type listKeyKey struct{}

func withListKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, listKeyKey{}, key)
}

func listKeyFrom(ctx context.Context) string {
	key, _ := ctx.Value(listKeyKey{}).(string)
	return key
}

// stepKeyIn is the key of the step id in the list ctx runs.
func stepKeyIn(ctx context.Context, id string) string {
	return joinStepKey(listKeyFrom(ctx), id)
}

func joinStepKey(prefix, key string) string {
	switch {
	case prefix == "":
		return key
	case key == "":
		return prefix
	}
	return prefix + "/" + key
}

// InStatementBody reports whether the Evaluator runs a statement body.
func (e *Evaluator) InStatementBody() bool { return e.statementMode() }

// ChildFrame is childFrame for the steps package: a clone whose names live in
// a new frame inside this one's.
func (e *Evaluator) ChildFrame() *Evaluator { return e.childFrame() }

// Bind binds a name in this Evaluator's current frame -- a loop variable.
func (e *Evaluator) Bind(name string, v any) {
	if e.names != nil {
		e.names.bind(name, viewRows(v))
	}
}

// withBodyRunner puts the runner for nested lists into ctx.
func (e *Executor) withBodyRunner(ctx context.Context, parent *StepContext) context.Context {
	var run bodyRunner
	run = func(ctx context.Context, steps []*Step, ev *Evaluator) (seqOutcome, error) {
		child := *parent
		child.Evaluator = ev
		return e.runSequence(ctx, steps, &sequenceRun{stepCtx: &child})
	}
	return context.WithValue(ctx, bodyRunnerKey{}, run)
}

// runSequence runs one list of statement steps (see the file comment).
func (e *Executor) runSequence(ctx context.Context, steps []*Step, run *sequenceRun) (seqOutcome, error) {
	stepCtx := run.stepCtx
	ev := stepCtx.Evaluator
	exec := stepCtx.Execution
	for _, s := range steps {
		if s != nil {
			ev.names.declare(s.Binds)
		}
	}
	ctx = e.withBodyRunner(ctx, stepCtx)

	for stepIndex, step := range steps {
		if step == nil {
			continue
		}
		if run.top || run.orders {
			exec.StepOrder = append(exec.StepOrder, step.ID)
		}
		if run.top {
			if err := ctx.Err(); err != nil {
				return seqOutcome{}, err
			}
			if run.cancelPoll != nil && run.cancelPoll.due(time.Now()) {
				if asked, by := run.journal.cancelRequested(ctx, exec.ID); asked {
					return seqOutcome{}, &runCancelled{by: by}
				}
			}
		}
		if r := run.resumed; r != nil {
			if m, ok := r.done[step.ID]; ok {
				ev.names.bind(step.Binds, viewRows(m.Value))
				continue
			}
			if r.continued[step.ID] {
				continue
			}
		}

		if step.Exprs != nil && step.Exprs.Condition != nil {
			shouldRun, err := ev.StepCondition(ctx, step)
			if err != nil {
				// As the legacy loop does: a condition that cannot be decided
				// does not run its step.
				if stepCtx.Logger != nil {
					stepCtx.Logger.Warn("step condition evaluation failed",
						"component", ComponentName, "step", step.ID, "error", err)
				}
				shouldRun = false
			}
			if !shouldRun {
				now := time.Now()
				skipped := &StepResult{StepId: step.ID, Status: "skipped", StartedAt: now, CompletedAt: now}
				e.recordStep(ctx, run, step, skipped)
				run.journalSkipped(ctx, step, stepIndex)
				continue
			}
		}

		switch step.Type {
		case StepTypeExpression, StepTypeReturn:
			// Evaluated here rather than through the registry, and journaled
			// like every other step: an intent row, then its receipt.
			run.journalRunning(ctx, step, stepIndex, 1)
			started := time.Now()
			var (
				v   any
				err error
			)
			if step.Exprs.Value != nil {
				v, err = ev.EvalV1(ctx, step.Exprs.Value)
			}
			res := &StepResult{StepId: step.ID, StartedAt: started, CompletedAt: time.Now()}
			res.Duration = res.CompletedAt.Sub(started)
			if err != nil {
				res.Status, res.Error = "failed", err.Error()
				e.recordStep(ctx, run, step, res)
				return seqOutcome{}, fmt.Errorf("step %q: %w", step.ID, err)
			}
			res.Status, res.Result = "success", unwrapStatementValue(v)
			if step.Type == StepTypeExpression {
				res.Bound = journaledValue(step, v)
				e.recordStep(ctx, run, step, res)
				ev.names.bind(step.Binds, v)
				continue
			}
			if memql.IsAbsent(res.Result) {
				res.Result = nil
			}
			e.recordStep(ctx, run, step, res)
			return seqOutcome{Returned: true, Value: res.Result}, nil
		}

		result, err := e.runStatementStep(ctx, step, stepIndex, run)
		if err != nil {
			var cancelled *runCancelled
			if errors.As(err, &cancelled) {
				return seqOutcome{}, err
			}
			if step.OnError == ErrorStrategyContinue {
				if stepCtx.Logger != nil {
					stepCtx.Logger.Warn("statement failed, continuing (on error continue)",
						"component", ComponentName, "step", step.ID, "error", err)
				}
				continue
			}
			return seqOutcome{}, err
		}
		// A return inside a loop's or a branch's list ends this list too.
		if result != nil && result.Metadata != nil {
			if returned, _ := result.Metadata[metaReturned].(bool); returned {
				return seqOutcome{Returned: true, Value: result.Result}, nil
			}
		}
		value := statementValue(step, result)
		ev.names.bind(step.Binds, value)
		if step.Returns {
			return seqOutcome{Returned: true, Value: unwrapStatementValue(value)}, nil
		}
	}
	return seqOutcome{}, nil
}

// metaReturned is the StepResult metadata key a list-holding step (a `for`,
// a block) sets when a return ended its list, with the value in Result.
const metaReturned = "returned"

// runStatementStep executes one step that runs through the step registry,
// retrying a failed attempt up to RetryCount more times -- `retry(n)` retries
// whatever `on error` says. The top-level list journals each attempt.
func (e *Executor) runStatementStep(ctx context.Context, step *Step, stepIndex int, run *sequenceRun) (*StepResult, error) {
	stepCtx := run.stepCtx
	exec := stepCtx.Execution
	attempts := 1 + step.RetryCount
	var (
		result *StepResult
		err    error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		if run.top {
			stepCtx.PreviousChainHead = run.chainHead
		}
		run.journalRunning(ctx, step, stepIndex, run.attemptBase(step)+attempt)
		result, err = e.executeJournaledStep(ctx, run.heartbeatJournal(), step, stepCtx)
		if result != nil && run.top && stepCtx.ChainTrackingEnabled {
			result.PreviousChainHead = run.chainHead
			result.ContentId = StepDeterministicFingerprint(step, result)
			run.chainHead = string(fingerprintEngine.Combine(id.ID(run.chainHead), id.ID(result.ContentId)))
		}
		if result != nil {
			if err == nil {
				// Before the receipt, which records it.
				result.Bound = journaledValue(step, statementValue(step, result))
			}
			e.recordStep(ctx, run, step, result)
		}
		if err == nil {
			return result, nil
		}
		var cancelled *runCancelled
		if errors.As(err, &cancelled) {
			return result, err
		}
		if attempt < attempts && stepCtx.Logger != nil {
			stepCtx.Logger.Info("retrying statement", "component", ComponentName, "step", step.ID, "attempt", attempt+1)
		}
	}
	if run.top {
		exec.RecordFailedStep(step, attempts)
	}
	return result, err
}

// recordStep puts a top-level step's result on the run and tells the
// observer, and writes the journal's receipt for a list that journals. A
// nested list's steps are the step that holds them -- a `for`, a block -- as a
// forEach's children always were: their ids are unique only within their own
// list, so on the run they would collide.
func (e *Executor) recordStep(ctx context.Context, run *sequenceRun, step *Step, result *StepResult) {
	if run.top {
		if exec := run.stepCtx.Execution; exec != nil {
			exec.AddStepResult(result)
		}
		notifyStepObserver(ctx, result)
	}
	if result.Status != "skipped" {
		run.journalFinished(ctx, step, result)
	}
}

// journalRunning writes a step's intent row, when this list journals.
func (r *sequenceRun) journalRunning(ctx context.Context, step *Step, seq, attempt int) {
	r.journal.stepRunning(ctx, r.stepCtx.Execution, step, seq, attempt)
}

// journalFinished writes a step's receipt, the way this list journals: a
// run's own list heartbeats its run; a logic's statements inside a caller's
// run write their rows only, the run row being the caller's.
func (r *sequenceRun) journalFinished(ctx context.Context, step *Step, result *StepResult) {
	if r.rowsOnly {
		r.journal.stepFinishedRowOnly(ctx, r.stepCtx.Execution, step, result)
		return
	}
	r.journal.stepFinished(ctx, r.stepCtx.Execution, step, result, r.chainHead)
}

// journalSkipped writes a skipped step's row, when this list journals.
func (r *sequenceRun) journalSkipped(ctx context.Context, step *Step, seq int) {
	r.journal.stepSkipped(ctx, r.stepCtx.Execution, step, seq)
}

// heartbeatJournal is the journal whose run a step's execution keeps alive:
// the list's own. A logic's statements inside a caller's run keep nothing
// alive: the calling statement's execution heartbeats that run already.
func (r *sequenceRun) heartbeatJournal() *workJournal {
	if r.rowsOnly {
		return nil
	}
	return r.journal
}

// statementValue is the value a step binds: a construct call's result shaped
// for reading (functionStatementValue), a sub-automation's returned value, an
// action's capability result; nil for a step that binds nothing.
func statementValue(step *Step, result *StepResult) any {
	if result == nil {
		return nil
	}
	switch step.Type {
	case StepTypeFunction:
		kind := ""
		if step.Function != nil {
			kind = step.Function.Kind
		}
		return functionStatementValue(kind, result.Result)
	case StepTypeAutomation:
		if sub, ok := result.Result.(*AutomationExecution); ok && sub != nil {
			return viewRows(sub.Output)
		}
		return viewRows(result.Result)
	case StepTypeAction:
		return viewRows(actionStatementValue(result.Result))
	}
	return viewRows(result.Result)
}

// actionStatementValue is what an action statement binds: the capability's
// own result, which the retired `.result.result.result` climb reached. An
// authored action's step result is its record of the call -- `{authored, ref,
// capability, result, resultFingerprint}` (steps/action.go executeAuthored) --
// whose `result` is the capability's output, and a capability script's output
// is an envelope (`{ok, changed, result, ...}`) whose `result` is the
// script's own: both unwrap. The record and the envelope stay on the step's
// result, which the journal keeps. A replayed action's record, `{replayed, ref,
// results}`, holds no single result and binds as it is.
func actionStatementValue(raw any) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	if authored, _ := m["authored"].(bool); authored {
		inner, has := m["result"]
		if !has {
			return m
		}
		if m, ok = inner.(map[string]any); !ok {
			return inner
		}
	}
	if inner, has := m["result"]; has {
		if _, env := m["ok"]; env {
			return inner
		}
	}
	return m
}

// runCancelled is the error a top-level sequence returns when somebody asked
// for the run to stop (journal.cancelRequested).
type runCancelled struct{ by string }

func (c *runCancelled) Error() string { return "run cancelled by " + c.by }

// runStatementAutomation runs an automation compiled from a statement body:
// its list through runSequence, then the run's close exactly as the legacy
// loop closes it -- the journal's terminal row, the chain head and dedup
// registration, the completed event -- plus the value a `return` ended it
// with, on the execution and in the run's outcome. resumed is nil for a
// fresh run; a resume (ResumeFrom) closes as the legacy resume does, with no
// dedup registration and no error hook, and says so on the completed event.
func (e *Executor) runStatementAutomation(ctx context.Context, automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event, journal *workJournal, stepCtx *StepContext, chainHead string, resumed *resumedList) (*AutomationExecution, error) {
	stepCtx.Evaluator.enterStatements()
	run := &sequenceRun{
		stepCtx:    stepCtx,
		top:        true,
		journal:    journal,
		chainHead:  chainHead,
		cancelPoll: newCancelPoller(e.cancelPollInterval),
		resumed:    resumed,
	}
	out, err := e.runSequence(ctx, automation.Steps, run)
	chainHead = run.chainHead
	if err != nil {
		var cancelled *runCancelled
		switch {
		case errors.As(err, &cancelled):
			// Somebody decided the work should stop; nothing failed.
			journal.cancelStop(ctx, exec, chainHead, cancelled.by)
			return exec, nil
		case ctx.Err() != nil && errors.Is(err, ctx.Err()):
			exec.Cancel()
			journal.closeRun(ctx, exec, chainHead)
			return exec, err
		}
		exec.Fail(err)
		journal.closeRun(ctx, exec, chainHead)
		if resumed == nil {
			e.handleAutomationError(ctx, automation, exec, triggeringEvent, err)
		}
		return exec, err
	}

	exec.Returned, exec.Output = out.Returned, out.Value
	exec.Complete()
	journal.closeRun(ctx, exec, chainHead)
	if e.chainTrackingEnabled {
		exec.ChainHead = chainHead
		if e.dedup != nil && resumed == nil {
			e.dedup.register(automation.Name, exec.InitialChainHead, exec.ID)
		}
	}
	completedPayload := map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"duration":       exec.Duration.Milliseconds(),
		"stepCount":      len(exec.Steps),
	}
	if resumed != nil {
		completedPayload["runId"] = exec.ID
		completedPayload["resumed"] = true
	}
	if e.chainTrackingEnabled && exec.ChainHead != "" {
		completedPayload["chainHead"] = exec.ChainHead
	}
	e.publishEvent(ctx, events.TopicAutomationCompleted, events.KindAutomationCompleted, completedPayload)
	return exec, nil
}
