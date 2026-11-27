package calls

import (
	"fmt"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
)

func workflowPauseForChange(r *callrules.WorkflowRun, agent bool, now time.Time) {
	workflowFreeze(r, now)
	r.Status = "paused"
	r.Revision++
	r.ControlEpoch++
	if !agent {
		r.Hold = "human"
	}
	workflowEvent(r, "change_paused", "Guidance paused while a replacement is checked. A failed change requires review before resuming.", now)
}

// Advisory time cannot promise more remaining time than the currently funded
// call. Pausing a step does not pause that deadline. Overtime may be reduced by
// an extension, but the resulting remaining time must still fit the call.
func workflowValidateExtension(duration, elapsed, seconds int64, deadline, now time.Time) error {
	if seconds < 1 || seconds > 3600 || duration+seconds > 14400 {
		return callrules.ErrInvalid
	}
	if deadline.IsZero() || !now.Before(deadline) || max(0, duration+seconds-elapsed) > int64(deadline.Sub(now)/time.Second) {
		return accountRefusal("workflow_extension_exceeds_call_deadline")
	}
	return nil
}

// Like the outer advisory overtime entry, each branch has one synthetic event
// per current visit. Its stable ID lets clients replace the increasing seconds
// instead of appending repeated Activity entries on every refresh. The caller
// applies the same host/role visibility filter as persisted events.
func workflowParallelOvertimeEvents(r callrules.WorkflowRun, now time.Time) []callrules.WorkflowEvent {
	events := []callrules.WorkflowEvent{}
	if r.Parallel == nil {
		return events
	}
	for _, b := range r.Parallel.Branches {
		elapsed := workflowBranchElapsed(&r, b, now)
		if b.DurationSeconds <= 0 || elapsed <= b.DurationSeconds {
			continue
		}
		n, ok := callrules.WorkflowNodeByID(r.Definition, b.NodeID)
		if !ok {
			continue
		}
		at := b.AnchorAt
		// Pin the marker to the visit's recorded entry, including while paused
		// (when the running anchor is deliberately zero). This is an advisory
		// marker, not a claim that a provider action occurred at this time.
		for _, event := range r.Events {
			if event.VisitID == b.VisitID && (event.Type == "branch_entered" || event.Type == "step_entered") {
				at = event.At
				break
			}
		}
		if at.IsZero() {
			at = r.StartedAt
		}
		events = append(events, callrules.WorkflowEvent{ID: b.VisitID + "-overtime", At: at, Type: "overtime", NodeID: b.NodeID, VisitID: b.VisitID, ParallelVisitID: r.Parallel.ControllerVisitID, BranchID: b.ID, Message: fmt.Sprintf("%d seconds over this branch's advisory time. End turn when ready.", elapsed-b.DurationSeconds), Visibility: n.Visibility, RoleID: n.RoleID})
	}
	return events
}
