package app

import (
	"fmt"
	"github.com/znasllc-io/memql/component/automations"
)

// The installed DSL selects the discussion prompt. Its execution authority is
// narrower than ordinary work: one native evidence/model operation and return.
// Refuse an expanded plan before any step can run, including after node recovery.
func approvalDiscussionTemplateRefusal(a *automations.Automation) error {
	if a == nil || !a.Template || len(a.Steps) != 2 || a.Loop != nil || a.Mode != nil || len(a.Preconditions) != 0 {
		return fmt.Errorf("approval discussion requires its bounded read-only template")
	}
	call, result := a.Steps[0], a.Steps[1]
	if call.Type != automations.StepTypeFunction || call.Function == nil || call.Function.Name != "askRunApprovalMessage" || call.OnError != "" && call.OnError != automations.ErrorStrategyStop || call.RetryCount != 0 || call.Condition != "" || result.Type != automations.StepTypeReturn || result.FieldWrite != nil {
		return fmt.Errorf("approval discussion may only analyze recorded evidence and return advice")
	}
	return nil
}
