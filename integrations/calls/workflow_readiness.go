package calls

import (
	callrules "github.com/znasllc-io/memql/component/calls"
	"slices"
	"time"
)

func workflowReadinessObserve(d callrules.WorkflowDefinition, nodeID, visitID string, missing []string, current **callrules.WorkflowReadinessWait, now time.Time) bool {
	node, _ := callrules.WorkflowNodeByID(d, nodeID)
	if node.Readiness == nil || len(missing) == 0 {
		changed := *current != nil
		*current = nil
		return changed
	}
	if *current != nil && (*current).VisitID == visitID {
		return false
	}
	*current = &callrules.WorkflowReadinessWait{VisitID: visitID, BlockedAt: now, Deadline: now.Add(time.Duration(node.Readiness.TimeoutSeconds) * time.Second)}
	return true
}
func workflowApplyReadiness(r *callrules.WorkflowRun, missing []string, now time.Time) {
	workflowReadinessObserve(r.Definition, r.NodeID, r.VisitID, missing, &r.Readiness, now)
	if len(missing) > 0 {
		workflowFreeze(r, now)
		r.Status = "blocked"
		r.Blockers = slices.Clone(missing)
	}
}
func workflowFallbackTarget(r *callrules.WorkflowRun, now time.Time) (string, error) {
	node, _ := callrules.WorkflowNodeByID(r.Definition, r.NodeID)
	if r.Parallel != nil || node.Readiness == nil || node.Readiness.FallbackNodeID == "" || r.Readiness == nil || r.Readiness.VisitID != r.VisitID || len(r.Blockers) == 0 || slices.Contains(r.Blockers, "insights_or_account_unavailable") || workflowTerminal(r) {
		return "", accountRefusal("workflow_fallback_unavailable")
	}
	if now.Before(r.Readiness.Deadline) {
		return "", accountRefusal("workflow_readiness_timeout_pending")
	}
	if !workflowAckReady(r) {
		return "", accountRefusal("workflow_acknowledgment_required")
	}
	target := node.Readiness.FallbackNodeID
	if !slices.Contains(callrules.WorkflowNext(r.Definition, r.NodeID), target) {
		return "", accountRefusal("workflow_fallback_unavailable")
	}
	owner := workflowRepeatOwner(callrules.WorkflowRepeatRegions(r.Definition), target)
	if r.Repeat != nil && owner != r.Repeat.NodeID || r.Repeat == nil && owner != "" {
		return "", accountRefusal("workflow_repeat_navigation")
	}
	return target, nil
}
func workflowVisitOutcome(action, from, to string) string {
	switch action {
	case "previous":
		return "revisited"
	case "seek":
		if from == to {
			return "revisited"
		}
		return "skipped"
	case "fallback":
		return "skipped"
	case "cancel", "change", "call_ended":
		return "abandoned"
	default:
		return "completed"
	}
}
func workflowCurrentVisit(r *callrules.WorkflowRun, action, target string, now time.Time) callrules.WorkflowVisit {
	visit := callrules.WorkflowVisit{NodeID: r.NodeID, VisitID: r.VisitID, ElapsedSeconds: callrules.WorkflowElapsed(*r, now), Outcome: workflowVisitOutcome(action, r.NodeID, target), Action: action}
	if r.Repeat != nil {
		visit.RepeatVisitID, visit.RepeatIteration = r.Repeat.VisitID, r.Repeat.Iteration
	}
	return visit
}
func workflowAbandonVisit(r *callrules.WorkflowRun, action string, now time.Time) {
	if r.VisitID == "" {
		return
	}
	for _, visit := range r.History {
		if visit.VisitID == r.VisitID {
			return
		}
	}
	workflowAppendVisit(r, workflowCurrentVisit(r, action, "", now))
}
func workflowValidReadiness(v *workflowState) bool {
	if v.Run == nil {
		return true
	}
	r := v.Run
	valid := func(nodeID, visitID string, wait *callrules.WorkflowReadinessWait, blockers []string) bool {
		if wait == nil {
			return true
		}
		node, ok := callrules.WorkflowNodeByID(r.Definition, nodeID)
		return ok && node.Readiness != nil && !workflowTerminal(r) && wait.VisitID == visitID && !wait.BlockedAt.IsZero() && wait.Deadline.Equal(wait.BlockedAt.Add(time.Duration(node.Readiness.TimeoutSeconds)*time.Second)) && len(blockers) > 0
	}
	if !valid(r.NodeID, r.VisitID, r.Readiness, r.Blockers) {
		return false
	}
	visits := append([]callrules.WorkflowVisit{}, r.History...)
	if r.Parallel != nil {
		for _, branch := range r.Parallel.Branches {
			if !valid(branch.NodeID, branch.VisitID, branch.Readiness, branch.Blockers) {
				return false
			}
			visits = append(visits, branch.History...)
		}
	}
	for _, visit := range visits {
		if visit.Outcome == "" && visit.Action == "" {
			continue
		}
		if !slices.Contains([]string{"completed", "skipped", "revisited", "abandoned"}, visit.Outcome) || !slices.Contains([]string{"next", "end_turn", "end_turn_mute", "previous", "seek", "fallback", "cancel", "change", "call_ended"}, visit.Action) {
			return false
		}
	}
	if p := r.PreviousRunExit; p != nil {
		if !callrules.ValidWorkflowID(p.RunID) || !callrules.ValidWorkflowID(p.NodeID) || !callrules.ValidWorkflowID(p.VisitID) || p.Outcome != "abandoned" || p.Action != "change" || p.At.IsZero() {
			return false
		}
	}
	return true
}
