package app

// automation_run_refusal.go -- the one place that answers "may this
// automation be run?".
//
// UNTAGGED on purpose. Two node types ask the question and they must not
// answer it differently: the mcp node's manual `run_automation`
// (mcp_automation_runner.go, //go:build mcp) and the agent node's work-run
// dispatcher (integrations_work_dispatch.go, //go:build agent). It lived in
// the first of those, so the second could not see it -- and the failure mode
// of a second copy is that a @disabled automation stays runnable on exactly
// one path, which is the shape of bug nobody looks for.

import (
	"fmt"

	"github.com/znasllc-io/memql/component/automations"
)

// automationRunRefusal is the #2605 analogue for automations
// (memql#2681): a @disabled automation is not runnable, on the manual MCP
// path as anywhere else. Dropping it from the advertised surface alone
// would leave it RUNNABLE by name -- worse than the function case, which
// at least refused at call time. Returns nil when the automation may run.
func automationRunRefusal(auto *automations.Automation) error {
	if auto == nil {
		return nil
	}
	if !auto.IsEnabled() {
		return fmt.Errorf("automation %q is @disabled and cannot be run; remove the @disabled annotation to re-enable it", auto.Name)
	}
	return nil
}
