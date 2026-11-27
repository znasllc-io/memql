package calls

import callrules "github.com/znasllc-io/memql/component/calls"

// Effect nodes reuse one existing reviewed native intent. They are not executable
// code, provider URLs or an automatic task runner; entering a node does nothing.
func workflowCanPrepareMuteNode(node callrules.WorkflowNode) bool {
	return node.Type == "turn" || node.Type == "effect" && node.Effect != nil && node.Effect.Action == "end_turn_mute"
}
