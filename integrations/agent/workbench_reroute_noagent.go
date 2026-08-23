//go:build !agent

package agent

import "context"

// rerouteWorkbenchMismatch is a no-op off the agent build, and that is correct
// rather than a stub: the fleet it would reroute to is `workerHost`, which the
// agent build is the only one that registers. A cognition or planner binary
// that saw a workbench mismatch has nowhere to send the call, so leaving the
// mismatch as the model's answer is the honest outcome -- the message already
// names what the action needs.
func (r *Replier) rerouteWorkbenchMismatch(
	_ context.Context,
	_ turnContext,
	_ string,
	_ string,
	_ map[string]any,
) (string, bool) {
	return "", false
}
