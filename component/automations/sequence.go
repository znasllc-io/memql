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
// that, as a forEach's children never did.

import (
	"context"
	"errors"
	"fmt"
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
	// top marks an automation's own list: it journals, polls for
	// cancellation and advances the chain head.
	top        bool
	journal    *workJournal
	chainHead  string
	cancelPoll *cancelPoller
}

// bodyRunnerKey carries the executor's sequence runner to the step executors
// that hold lists of their own (a `for`, a parallel branch), which live in the
// steps package and so reach it through the context.
type bodyRunnerKey struct{}

type bodyRunner func(ctx context.Context, steps []*Step, ev *Evaluator) (seqOutcome, error)

// RunStatementBody runs a nested statement list -- a loop iteration's body, a
// parallel branch -- through the sequence runner of the automation it belongs
// to, over ev (a child frame the caller opened). It returns whether a return
// ended the list and with what value.
func RunStatementBody(ctx context.Context, steps []*Step, stepCtx *StepContext, ev *Evaluator) (bool, any, error) {
	run, ok := ctx.Value(bodyRunnerKey{}).(bodyRunner)
	if !ok || run == nil {
		return false, nil, fmt.Errorf("a statement body's nested steps ran outside a statement run")
	}
	out, err := run(ctx, steps, ev)
	return out.Returned, out.Value, err
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
		if run.top {
			exec.StepOrder = append(exec.StepOrder, step.ID)
			if err := ctx.Err(); err != nil {
				return seqOutcome{}, err
			}
			if run.cancelPoll != nil && run.cancelPoll.due(time.Now()) {
				if asked, by := run.journal.cancelRequested(ctx, exec.ID); asked {
					return seqOutcome{}, &runCancelled{by: by}
				}
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
				if run.top {
					run.journal.stepSkipped(ctx, exec, step, stepIndex)
				}
				continue
			}
		}

		switch step.Type {
		case StepTypeExpression:
			started := time.Now()
			v, err := ev.EvalV1(ctx, step.Exprs.Value)
			res := &StepResult{StepId: step.ID, StartedAt: started, CompletedAt: time.Now()}
			res.Duration = res.CompletedAt.Sub(started)
			if err != nil {
				res.Status, res.Error = "failed", err.Error()
				e.recordStep(ctx, run, step, res)
				return seqOutcome{}, fmt.Errorf("step %q: %w", step.ID, err)
			}
			res.Status, res.Result = "success", unwrapStatementValue(v)
			e.recordStep(ctx, run, step, res)
			ev.names.bind(step.Binds, v)
			continue

		case StepTypeReturn:
			started := time.Now()
			var v any
			if step.Exprs.Value != nil {
				var err error
				if v, err = ev.EvalV1(ctx, step.Exprs.Value); err != nil {
					return seqOutcome{}, fmt.Errorf("step %q: %w", step.ID, err)
				}
			}
			v = unwrapStatementValue(v)
			if memql.IsAbsent(v) {
				v = nil
			}
			res := &StepResult{StepId: step.ID, Status: "success", Result: v, StartedAt: started, CompletedAt: time.Now()}
			e.recordStep(ctx, run, step, res)
			return seqOutcome{Returned: true, Value: v}, nil
		}

		result, err := e.runStatementStep(ctx, step, stepIndex, run)
		if err != nil {
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
			run.journal.stepRunning(ctx, exec, step, stepIndex, attempt)
			result, err = e.executeJournaledStep(ctx, run.journal, step, stepCtx)
		} else {
			result, err = e.executeStep(ctx, step, stepCtx)
		}
		if result != nil && run.top && stepCtx.ChainTrackingEnabled {
			result.PreviousChainHead = run.chainHead
			result.ContentId = StepDeterministicFingerprint(step, result)
			run.chainHead = string(fingerprintEngine.Combine(id.ID(run.chainHead), id.ID(result.ContentId)))
		}
		if result != nil {
			e.recordStep(ctx, run, step, result)
		}
		if err == nil {
			return result, nil
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

// recordStep puts a top-level step's result on the run, tells the observer
// and writes the journal's receipt. A nested list's steps are the step that
// holds them -- a `for`, a block -- as a forEach's children always were: their
// ids are unique only within their own list, so on the run they would collide.
func (e *Executor) recordStep(ctx context.Context, run *sequenceRun, step *Step, result *StepResult) {
	if !run.top {
		return
	}
	exec := run.stepCtx.Execution
	if exec != nil {
		exec.AddStepResult(result)
	}
	notifyStepObserver(ctx, result)
	if result.Status != "skipped" {
		run.journal.stepFinished(ctx, exec, step, result, run.chainHead)
	}
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
// own result. A capability-script envelope (`{ok, changed, result, ...}`)
// unwraps to its `result`; the envelope's other fields stay on the step's
// metadata, which is what retires the `.result.result.result` climb.
func actionStatementValue(raw any) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return raw
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
// with, on the execution and in the run's outcome.
func (e *Executor) runStatementAutomation(ctx context.Context, automation *Automation, exec *AutomationExecution, triggeringEvent *events.Event, journal *workJournal, stepCtx *StepContext, chainHead string) (*AutomationExecution, error) {
	stepCtx.Evaluator.enterStatements()
	run := &sequenceRun{
		stepCtx:    stepCtx,
		top:        true,
		journal:    journal,
		chainHead:  chainHead,
		cancelPoll: newCancelPoller(e.cancelPollInterval),
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
		e.handleAutomationError(ctx, automation, exec, triggeringEvent, err)
		return exec, err
	}

	exec.Returned, exec.Output = out.Returned, out.Value
	exec.Complete()
	journal.closeRun(ctx, exec, chainHead)
	if e.chainTrackingEnabled {
		exec.ChainHead = chainHead
		if e.dedup != nil {
			e.dedup.register(automation.Name, exec.InitialChainHead, exec.ID)
		}
	}
	completedPayload := map[string]any{
		"automationName": automation.Name,
		"executionId":    exec.ID,
		"duration":       exec.Duration.Milliseconds(),
		"stepCount":      len(exec.Steps),
	}
	if e.chainTrackingEnabled && exec.ChainHead != "" {
		completedPayload["chainHead"] = exec.ChainHead
	}
	e.publishEvent(events.TopicAutomationCompleted, events.KindAutomationCompleted, completedPayload)
	return exec, nil
}
