package steps

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/harness/actionpin"
	"github.com/znasllc-io/memql/component/harness/actionreplay"
	"github.com/znasllc-io/memql/component/harness/parambind"
)

// ActionExecutor replays an action-library action referenced by a step
// (#1758, epic #1734). It resolves the action by its pinned-by-default
// reference, re-binds the action's parameters from the step's args, and
// replays the captured capability calls via the engine -- no LLM. This is the
// authored DSL surface for the merged component/harness/action* machinery
// (Phases 0-5): a plan or automation can now name a library action directly.
//
// Surface resolution + the consolidation lifecycle remain on the merged
// surfaceresolver / actiontrust packages; this executor performs the literal +
// parameterized replay against the default surface.
type ActionExecutor struct{}

// Execute runs an action step.
func (e *ActionExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	result := &automations.StepResult{
		StepId:    step.ID,
		StartedAt: time.Now(),
	}
	fail := func(msg string) (*automations.StepResult, error) {
		result.Status = "failed"
		result.Error = msg
		result.CompletedAt = time.Now()
		result.Duration = result.CompletedAt.Sub(result.StartedAt)
		return result, fmt.Errorf("%s", msg)
	}

	if step.Action == nil {
		return fail("action configuration is required")
	}
	if stepCtx.Engine == nil {
		return fail("MemQL engine not configured")
	}

	ref, err := actionpin.Parse(step.Action.Ref)
	if err != nil {
		return fail(fmt.Sprintf("invalid action ref %q: %v", step.Action.Ref, err))
	}

	// The step's args are the input the action's parameters re-bind from.
	input := map[string]any{}
	for k, v := range step.Action.Args {
		input[k] = v
	}
	if stepCtx.Evaluator != nil {
		if resolved, rerr := resolveArgsRefs(input, stepCtx.Evaluator); rerr == nil {
			input = resolved
		}
	}

	if stepCtx.Logger != nil {
		stepCtx.Logger.Debug("executing action step", "step", step.ID, "ref", step.Action.Ref)
	}

	results, rerr := e.replayActionRef(ctx, stepCtx, ref, input, 0)
	if rerr != nil {
		return fail(fmt.Sprintf("action %q replay failed: %v", step.Action.Ref, rerr))
	}

	result.Status = "success"
	result.Result = map[string]any{
		"replayed": true,
		"ref":      step.Action.Ref,
		"results":  results,
	}
	result.CompletedAt = time.Now()
	result.Duration = result.CompletedAt.Sub(result.StartedAt)
	return result, nil
}

// maxCompositeDepth bounds composite recursion so a cyclic composite (a
// composite that references itself, directly or transitively) cannot spin.
const maxCompositeDepth = 8

// replayActionRef resolves an action by reference and replays it, recursing
// into a composite's child action refs. A primitive replays its captured
// capability calls (re-bound from input); a composite walks its ordered child
// steps, binds each child's input from the composite's argTemplate, and
// recurses. Returns the concatenated child results.
func (e *ActionExecutor) replayActionRef(ctx context.Context, stepCtx *Context, ref actionpin.Ref, input map[string]any, depth int) ([]any, error) {
	if depth > maxCompositeDepth {
		return nil, fmt.Errorf("composite action nesting exceeds %d (cycle?)", maxCompositeDepth)
	}

	var query string
	if ref.Floating {
		query = fmt.Sprintf(`actionById({actionId:%q})`, ref.ID)
	} else {
		query = fmt.Sprintf(`actionByIdAndVersion({actionId:%q, version:%d})`, ref.ID, ref.Version)
	}
	payload, ok := resolveActionPayload(ctx, stepCtx, query)
	if !ok {
		return nil, fmt.Errorf("action %q not found", actionpin.Format(ref))
	}

	if kind, _ := payload["kind"].(string); kind == "composite" {
		return e.replayComposite(ctx, stepCtx, payload, input, depth)
	}

	// primitive: replay the captured calls, re-binding params from input.
	calls := actionReplayCalls(payload)
	if len(calls) == 0 {
		return nil, fmt.Errorf("action %q has no replayable calls", actionpin.Format(ref))
	}
	rebound := make([]actionreplay.ReplayCall, 0, len(calls))
	for _, c := range calls {
		rebound = append(rebound, actionreplay.ReplayCall{
			Index:      c.Index,
			Capability: c.Capability,
			Args:       parambind.BindArgs(c, input),
		})
	}
	results, _, rerr := actionreplay.Replay(ctx, stepCtx.Engine, rebound)
	return results, rerr
}

// replayComposite walks a composite action's ordered child steps, binding each
// child's input from the composite input via the step's argTemplate, and
// recurses. Child results are concatenated in step order.
func (e *ActionExecutor) replayComposite(ctx context.Context, stepCtx *Context, payload map[string]any, input map[string]any, depth int) ([]any, error) {
	stepsRaw, ok := payload["steps"].([]any)
	if !ok || len(stepsRaw) == 0 {
		return nil, fmt.Errorf("composite action has no steps")
	}
	var all []any
	for i, s := range stepsRaw {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		childRef, rerr := compositeChildRef(sm)
		if rerr != nil {
			return nil, fmt.Errorf("composite step %d: %w", i, rerr)
		}
		childInput := bindChildArgs(sm["argTemplate"], input)
		res, err := e.replayActionRef(ctx, stepCtx, childRef, childInput, depth+1)
		if err != nil {
			return nil, fmt.Errorf("composite step %d (%s): %w", i, actionpin.Format(childRef), err)
		}
		all = append(all, res...)
	}
	return all, nil
}

// compositeChildRef builds a child action reference from a composite step
// entry {actionId, version?}. A present version >= 1 pins it; otherwise the
// child floats to the latest version.
func compositeChildRef(sm map[string]any) (actionpin.Ref, error) {
	id, _ := sm["actionId"].(string)
	if id == "" {
		return actionpin.Ref{}, fmt.Errorf("missing actionId")
	}
	if v, ok := sm["version"].(float64); ok && v >= 1 {
		return actionpin.Ref{ID: id, Version: int(v)}, nil
	}
	return actionpin.Ref{ID: id, Floating: true}, nil
}

// bindChildArgs maps a composite step's argTemplate to the child's input. An
// argTemplate value that is a string naming a key in the composite input binds
// to that input value (composite param -> child arg); any other value passes
// through literally. Pure + unit-tested.
func bindChildArgs(tmpl any, input map[string]any) map[string]any {
	out := map[string]any{}
	m, ok := tmpl.(map[string]any)
	if !ok {
		return out
	}
	for childKey, v := range m {
		if s, ok := v.(string); ok {
			if bound, present := input[s]; present {
				out[childKey] = bound
				continue
			}
		}
		out[childKey] = v
	}
	return out
}

// resolveActionPayload runs the resolution query and returns the first matching
// action row's full payload.
func resolveActionPayload(ctx context.Context, stepCtx *Context, query string) (map[string]any, bool) {
	res, err := stepCtx.Engine.Execute(ctx, query)
	if err != nil || res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return nil, false
	}
	node := res.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return nil, false
	}
	return node.Payload.AsMap(), true
}

// actionReplayCalls reconstructs the parameterized replay calls from a stored
// action payload. It prefers the parameterized paramBindings (so step args
// re-bind on replay); when an action carries none, it falls back to the
// literal recorded calls (all bindings literal).
func actionReplayCalls(payload map[string]any) []parambind.Call {
	if pb, ok := payload["paramBindings"].([]any); ok && len(pb) > 0 {
		return parseActionParamBindings(pb)
	}
	if cs, ok := payload["calls"].([]any); ok {
		return parseActionLiteralCalls(cs)
	}
	return nil
}

func parseActionParamBindings(arr []any) []parambind.Call {
	out := make([]parambind.Call, 0, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		capability, _ := m["capability"].(string)
		idx := i
		if f, ok := m["index"].(float64); ok {
			idx = int(f)
		}
		call := parambind.Call{Index: idx, Capability: capability}
		if barr, ok := m["bindings"].([]any); ok {
			for _, be := range barr {
				bm, ok := be.(map[string]any)
				if !ok {
					continue
				}
				name, _ := bm["name"].(string)
				kind, _ := bm["kind"].(string)
				ref, _ := bm["ref"].(string)
				frag, _ := bm["fragment"].(bool)
				tmpl, _ := bm["template"].(string)
				fragVal, _ := bm["fragmentValue"].(string)
				call.Bindings = append(call.Bindings, parambind.Binding{
					Name:          name,
					Kind:          parambind.SourceKind(kind),
					Ref:           ref,
					Fragment:      frag,
					Template:      tmpl,
					FragmentValue: fragVal,
					Literal:       bm["literal"],
				})
			}
		}
		out = append(out, call)
	}
	return out
}

func parseActionLiteralCalls(arr []any) []parambind.Call {
	out := make([]parambind.Call, 0, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		capability, _ := m["capability"].(string)
		idx := i
		if f, ok := m["index"].(float64); ok {
			idx = int(f)
		}
		call := parambind.Call{Index: idx, Capability: capability}
		if args, ok := m["args"].(map[string]any); ok {
			for name, v := range args {
				call.Bindings = append(call.Bindings, parambind.Binding{
					Name:    name,
					Kind:    parambind.SourceLiteral,
					Literal: v,
				})
			}
		}
		out = append(out, call)
	}
	return out
}
