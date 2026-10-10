package planner

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations"
)

// The frozen Spine declares which invocation substitutions would drop a
// required capability. Compare compiled argument expressions, without executing
// them or trusting a model's claim that a text-only replacement has tools.
func replanPreservesInvocation(original, revised *automations.Automation, policy any) error {
	if policy == nil {
		return nil
	}
	var rules []struct{ From, To, Argument string }
	raw, err := json.Marshal(policy)
	if err != nil || json.Unmarshal(raw, &rules) != nil || len(rules) > 16 {
		return fmt.Errorf("invalid invocation preservation policy")
	}
	for _, rule := range rules {
		if rule.From == "" || rule.To == "" || rule.Argument == "" || rule.From == rule.To {
			return fmt.Errorf("incomplete invocation preservation policy")
		}
		values := map[string]bool{}
		existing := map[string]int{}
		visitReplanCalls(original.Steps, func(call *automations.FunctionStepConfig) {
			if call.Args[rule.Argument] != nil {
				value, _ := json.Marshal(call.Args[rule.Argument])
				if replanCallName(call) == rule.From {
					values[string(value)] = true
				} else if replanCallName(call) == rule.To {
					existing[string(value)]++
				}
			}
		})
		var violation bool
		visitReplanCalls(revised.Steps, func(call *automations.FunctionStepConfig) {
			if replanCallName(call) == rule.To && call.Args[rule.Argument] != nil {
				value, _ := json.Marshal(call.Args[rule.Argument])
				if existing[string(value)] > 0 {
					existing[string(value)]--
				} else {
					violation = violation || values[string(value)]
				}
			}
		})
		if violation {
			return fmt.Errorf("replan cannot replace %s with %s for the same %s; preserve the required execution capability", rule.From, rule.To, rule.Argument)
		}
	}
	return nil
}

func replanCallName(call *automations.FunctionStepConfig) string {
	if call.Kind != "builtin" {
		return ""
	}
	return call.Name[strings.LastIndex(call.Name, ".")+1:]
}

func visitReplanCalls(steps []*automations.Step, visit func(*automations.FunctionStepConfig)) {
	for _, step := range steps {
		if step.Function != nil {
			visit(step.Function)
		}
		if step.ForEach != nil {
			visitReplanCalls(step.ForEach.Do, visit)
		}
		if step.Parallel != nil {
			visitReplanCalls(step.Parallel.Branches, visit)
		}
		if step.Block != nil {
			visitReplanCalls(step.Block.Steps, visit)
		}
	}
}
