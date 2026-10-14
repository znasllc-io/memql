package work

import "github.com/znasllc-io/memql/component/automations/workflowhost"

// Only the external proving host owns these ports. The installation declares
// them for strict boot checks; serialized builtin arguments confer no scope.
func evaluationPorts() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"goalEvaluationPrepare": nil, "goalEvaluationSubmit": nil,
		"goalEvaluationObserve": nil, "goalEvaluationVerify": nil,
		"goalEvaluationRecord": nil,
	}
}
