package automations

import (
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
)

func (e *Executor) stepRetryable(step *Step) bool {
	var functions *memql.FunctionRegistry
	var specs *memql.SpecRegistry
	if e.engine != nil {
		functions, specs = e.engine.Functions(), e.engine.Specs()
	}
	return replaySafeStep(step, functions, specs, map[string]bool{}, 0)
}

// The authored kind controls binding, not dispatch. Classify the actual
// registered callee and every nested statement. A composer is not itself
// evidence that its body can be repeated.
func replaySafeStep(step *Step, functions *memql.FunctionRegistry, specs *memql.SpecRegistry, visiting map[string]bool, depth int) bool {
	if step == nil || depth > 64 {
		return false
	}
	var children []*Step
	switch step.Type {
	case StepTypeFunction:
		if step.Function == nil || functions == nil || strings.EqualFold(step.Function.Kind, "mutation") {
			return false
		}
		fn, found := functions.Lookup(step.Function.Name)
		if !found || fn == nil {
			return false
		}
		if fn.FunctionKind != "logic" {
			return memql.ReplayReadOnly(functions, specs, step.Function.Name)
		}
		if fn.LogicBody == nil || visiting[fn.Name] {
			return false
		}
		visiting[fn.Name] = true
		defer delete(visiting, fn.Name)
		data, err := json.Marshal(map[string]any{"name": "logic:" + fn.Name, "steps": fn.LogicBody})
		if err != nil {
			return false
		}
		body, err := NewLoader(LoaderOptions{Functions: functions}).parseJSON(data, "logic:"+fn.Name)
		if err != nil {
			return false
		}
		children = body.Steps
	case StepTypeForEach:
		if step.ForEach == nil {
			return false
		}
		children = step.ForEach.Do
	case StepTypeParallel:
		if step.Parallel == nil {
			return false
		}
		children = step.Parallel.Branches
	case StepTypeBlock:
		if step.Block == nil {
			return false
		}
		children = step.Block.Steps
	default:
		return IsStepRetryable(step.Type)
	}
	for _, child := range children {
		if !replaySafeStep(child, functions, specs, visiting, depth+1) {
			return false
		}
	}
	return true
}
