package automations

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

// ExecuteInScope interprets a callable template within a native operation that
// has already been admitted and owns its effects. Unlike firing an automation,
// this neither opens nor closes a work run, injects a system actor, replaces a
// step's model-call association, nor applies event admission a second time.
// The caller MUST supply a restricted registry and retain authorization,
// cancellation, effect journaling and recovery. This is a Go runtime seam,
// never an alternative wire entry point for arbitrary submitted DSL.
func ExecuteInScope(ctx context.Context, a *Automation, args map[string]any, opts ExecutorOptions) (*AutomationExecution, error) {
	if a == nil || !a.Template || !a.IsEnabled() || a.BeforeWrite != nil || a.IsScheduled() || a.IsEventTriggered() {
		return nil, fmt.Errorf("scoped execution requires an enabled callable template")
	}
	if opts.StepRegistry == nil || a.JournalRequired || a.Mode != nil || a.Loop != nil {
		return nil, fmt.Errorf("scoped execution requires bounded operations and an externally owned journal")
	}
	if err := ensurePrepared(a); err != nil {
		return nil, err
	}
	ev := events.NewEvent("workflow.call", events.KindMessage, args)
	bound, _, err := bindEventArgs(a, &ev)
	if err != nil {
		return nil, fmt.Errorf("workflow %s arguments: %w", a.Name, err)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	r := &LogicRunner{engine: opts.Engine, logger: opts.Logger}
	evaluator := r.newEvaluatorForLogic(ctx, bound)
	evaluator.enterStatements()
	evaluator.SetCustom("event", buildEventEnvelope(&ev, "workflow-scope", ev.Topic))
	execution := NewExecution(a.Name, "workflow-scope")
	if missed, isMiss := EvaluatePreconditions(a.Preconditions, evaluator); isMiss {
		err := fmt.Errorf("workflow %s precondition %q missed", a.Name, missed.ID)
		execution.PreconditionMissed = true
		execution.Fail(err)
		return execution, err
	}
	// As in LogicRunner, inherit actual caller authority; a template's source
	// or arguments cannot upgrade it.
	execution.SourceTrusted = auth.OriginFromContext(ctx).IsInternal()
	executor := &Executor{logger: opts.Logger, stepRegistry: opts.StepRegistry, borrowedRun: true}
	stepCtx := &StepContext{Logger: opts.Logger, Evaluator: evaluator, Execution: execution, AutomationTrigger: opts.AutomationTrigger}
	out, err := executor.runSequence(ctx, a.Steps, &sequenceRun{stepCtx: stepCtx})
	if err != nil {
		execution.Fail(err)
		return execution, err
	}
	execution.Returned, execution.Output = out.Returned, out.Value
	execution.Complete()
	return execution, nil
}
