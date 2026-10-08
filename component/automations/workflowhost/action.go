package workflowhost

import (
	"context"
	"fmt"
	"slices"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/actions/pin"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
)

func loadAction(ref string) (*actions.Action, error) {
	return sourceAction(installedSource, ref)
}

func sourceAction(source SourceLoader, name string) (*actions.Action, error) {
	ref, err := pin.Parse(name)
	if err != nil {
		return nil, err
	}
	body, err := source("action", ref.ID)
	if err != nil {
		return nil, err
	}
	loaded, err := actions.LoadSource(body, "workflow:"+name)
	if err != nil {
		return nil, err
	}
	if len(loaded) != 1 || loaded[0].Name != ref.ID || (!ref.Floating && loaded[0].Version != ref.Version) {
		return nil, fmt.Errorf("invalid workflow action %q", name)
	}
	return loaded[0], nil
}

func (h *host) prepareAction(step *automations.Step) error {
	if step.Action == nil || step.Action.Surface != "" {
		return fmt.Errorf("workflow actions use their host's authorized execution surface")
	}
	ref := step.Action.Ref
	if h.actions[ref] != nil {
		return nil
	}
	a, err := h.opts.LoadAction(ref)
	if err != nil {
		return err
	}
	if a == nil || !a.Enabled || h.opts.Operations[a.Capability] == nil {
		return fmt.Errorf("workflow action %q has no scoped capability", ref)
	}
	// Freeze both the reference and every literal before the first effect.
	// A caller-supplied registry may change while this invocation is running.
	frozen := *a
	frozen.Params = slices.Clone(a.Params)
	frozen.CallArgs = slices.Clone(a.CallArgs)
	for i := range frozen.CallArgs {
		frozen.CallArgs[i].Literal = cloneLiteral(frozen.CallArgs[i].Literal)
	}
	reg := actions.NewRegistry()
	if err := reg.Register(&frozen); err != nil {
		return err
	}
	if h.actions == nil {
		h.actions = map[string]*actions.Registry{}
	}
	h.actions[ref] = reg
	return nil
}

func cloneLiteral(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, value := range v {
			out[k] = cloneLiteral(value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = cloneLiteral(value)
		}
		return out
	default:
		return v
	}
}

type actionDispatcher struct {
	operations map[string]Operation
	err        error
}

func (d *actionDispatcher) Invoke(ctx context.Context, name string, args map[string]any) (any, error) {
	op := d.operations[name]
	if op == nil {
		d.err = fmt.Errorf("action capability %q is outside this workflow scope", name)
		return nil, d.err
	}
	value, err := op(ctx, args)
	d.err = err
	return value, err
}

func (h *host) executeAction(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d := &actionDispatcher{operations: h.opts.Operations}
	e := steps.ActionExecutor{Registry: h.actions[step.Action.Ref], Dispatcher: d}
	result, err := e.Execute(ctx, step, sc)
	if d.err != nil {
		return result, d.err // preserve the native typed refusal across the interpreter
	}
	return result, err
}
