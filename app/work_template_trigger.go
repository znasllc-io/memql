package app

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

type workTemplateTrigger struct {
	app      *App
	template *workTemplate
}
type workTemplateStackKey struct{}

func (t *workTemplateTrigger) executor() *automations.Executor {
	var registry automations.StepExecutorRegistry = t.app.stepRegistry
	if t.template.prepared != nil {
		registry = t.template.prepared.StepRegistry()
	}
	return automations.NewExecutor(automations.ExecutorOptions{Logger: t.app.Logger, Engine: t.app.engine, EventBus: t.app.eventBus, StepRegistry: registry, AutomationTrigger: t})
}
func (t *workTemplateTrigger) TriggerAutomation(ctx context.Context, name string) (*automations.AutomationExecution, error) {
	return t.TriggerAutomationWithArgs(ctx, name, nil)
}
func (t *workTemplateTrigger) TriggerAutomationWithArgs(ctx context.Context, name string, args map[string]any) (*automations.AutomationExecution, error) {
	ctx, err := t.EnterAutomationInvocation(ctx, name)
	if err != nil {
		return nil, err
	}
	auto, err := t.ResolveAutomation(ctx, name)
	if err != nil {
		return nil, err
	}
	executor := t.executor()
	defer executor.Close()
	result, err := executor.ExecuteWithClientEvent(ctx, auto, "work.subtemplate", &events.Event{Payload: args})
	if err == nil && result.Status != "completed" {
		err = fmt.Errorf("work subtemplate %s ended %s: %s", name, result.Status, result.Error)
	}
	return result, err
}

// ResolveAutomation uses the same pinned draft members and installed fallback
// for an initial invocation and a saved child continuation.
func (t *workTemplateTrigger) ResolveAutomation(ctx context.Context, name string) (*automations.Automation, error) {
	if t.template.prepared != nil {
		return t.template.prepared.Automation(name)
	}
	auto := t.template.members[name]
	if auto == nil {
		var err error
		auto, err = t.app.automationLoader.LoadByName(name)
		if err != nil {
			return nil, err
		}
	}
	if err := automationRunRefusal(auto); err != nil {
		return nil, err
	}
	return auto, nil
}

func (t *workTemplateTrigger) EnterAutomationInvocation(ctx context.Context, name string) (context.Context, error) {
	stack, _ := ctx.Value(workTemplateStackKey{}).([]string)
	if len(stack) >= 16 {
		return nil, fmt.Errorf("work template invocation nesting exceeds 16")
	}
	for _, caller := range stack {
		if caller == name {
			return nil, fmt.Errorf("recursive work template invocation: %s", name)
		}
	}
	next := append(append([]string(nil), stack...), name)
	return context.WithValue(ctx, workTemplateStackKey{}, next), nil
}
