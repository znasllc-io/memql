package calls

import (
	callrules "github.com/znasllc-io/memql/component/calls"
	"slices"
	"strconv"
	"strings"
	"time"
)

func workflowRepeatOwner(regions map[string]callrules.WorkflowRepeatRegion, node string) string {
	for id, region := range regions {
		if region.Body[node] {
			return id
		}
	}
	return ""
}
func workflowForwardTargets(r *callrules.WorkflowRun) []string {
	if r.Parallel != nil {
		if workflowParallelJoined(r) {
			return []string{callrules.WorkflowParallelRegions(r.Definition)[r.NodeID].AfterID}
		}
		return nil
	}
	if r.Repeat != nil {
		region, ok := callrules.WorkflowRepeatRegions(r.Definition)[r.Repeat.NodeID]
		if ok && r.NodeID == region.ExitID {
			if r.Repeat.Iteration < region.Count {
				return []string{region.EntryID}
			}
			return []string{region.AfterID}
		}
	}
	return callrules.WorkflowNext(r.Definition, r.NodeID)
}
func workflowRepeatRevisit(r *callrules.WorkflowRun, visit callrules.WorkflowVisit, regions map[string]callrules.WorkflowRepeatRegion) bool {
	if visit.ParallelVisitID != "" {
		return false
	}
	owner := workflowRepeatOwner(regions, visit.NodeID)
	if r.Repeat == nil {
		return owner == ""
	}
	return owner == r.Repeat.NodeID && visit.RepeatVisitID == r.Repeat.VisitID && visit.RepeatIteration == r.Repeat.Iteration
}
func workflowCanSeek(r *callrules.WorkflowRun, target string) bool {
	if workflowParallelOwner(r.Definition, target) != "" {
		return false
	}
	regions := callrules.WorkflowRepeatRegions(r.Definition)
	if target == r.NodeID {
		return r.Repeat != nil || workflowRepeatOwner(regions, target) == ""
	}
	for _, visit := range r.History {
		if visit.NodeID == target && workflowRepeatRevisit(r, visit, regions) {
			return true
		}
	}
	return false
}
func workflowSeekableNodes(r *callrules.WorkflowRun) []string {
	out := []string{}
	for _, node := range r.Definition.Nodes {
		if workflowCanSeek(r, node.ID) {
			out = append(out, node.ID)
		}
	}
	return out
}

// Record the outgoing visit before changing the round. Otherwise the immutable
// visit journal would incorrectly label an exit as part of the next iteration.
func workflowMove(r *callrules.WorkflowRun, target string, advance bool, now time.Time) bool {
	regions := callrules.WorkflowRepeatRegions(r.Definition)
	var next *callrules.WorkflowRepeatProgress
	if r.Repeat != nil {
		copy := *r.Repeat
		next = &copy
	}
	if !advance && !workflowCanSeek(r, target) {
		return false
	}
	if advance {
		if !slices.Contains(workflowForwardTargets(r), target) {
			return false
		}
		if region, ok := regions[r.NodeID]; ok {
			if r.Repeat != nil || target != region.EntryID {
				return false
			}
			next = &callrules.WorkflowRepeatProgress{NodeID: region.ControllerID, VisitID: r.VisitID, Iteration: 1, Count: region.Count}
		} else if r.Repeat != nil {
			region, ok := regions[r.Repeat.NodeID]
			if !ok {
				return false
			}
			if r.NodeID == region.ExitID {
				if r.Repeat.Iteration < region.Count {
					next.Iteration++
				} else {
					next = nil
				}
			}
		}
	}
	workflowEnter(r, target, now)
	r.Repeat = next
	return true
}
func workflowRepeatVisitNumber(run, id string) int64 {
	prefix := run + "-v"
	if !strings.HasPrefix(id, prefix) {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(id, prefix), 10, 64)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// An activation may age out of the bounded journal after explicit revisits. If
// its visit number is still in the retained range, it must be the exact controller
// visit, never an unrelated body visit borrowed as a new round authority.
func workflowRepeatActivation(r *callrules.WorkflowRun, controller, activation string) bool {
	number := workflowRepeatVisitNumber(r.ID, activation)
	if number == 0 || number >= r.VisitNumber {
		return false
	}
	earliest := r.VisitNumber
	for _, visit := range r.History {
		n := workflowRepeatVisitNumber(r.ID, visit.VisitID)
		if n > 0 && n < earliest {
			earliest = n
		}
		if visit.VisitID == activation {
			return visit.NodeID == controller && visit.RepeatVisitID == "" && visit.RepeatIteration == 0
		}
	}
	return len(r.History) == 256 && number < earliest
}
func workflowValidRepeat(v *workflowState) bool {
	if v.Kind != "run" || v.Run == nil {
		return true
	}
	r := v.Run
	regions := callrules.WorkflowRepeatRegions(r.Definition)
	for _, visit := range r.History {
		owner := workflowRepeatOwner(regions, visit.NodeID)
		if owner == "" {
			if visit.RepeatVisitID != "" || visit.RepeatIteration != 0 {
				return false
			}
			continue
		}
		origin := workflowRepeatVisitNumber(r.ID, visit.RepeatVisitID)
		if origin == 0 || origin >= workflowRepeatVisitNumber(r.ID, visit.VisitID) || !workflowRepeatActivation(r, owner, visit.RepeatVisitID) || visit.RepeatIteration < 1 || visit.RepeatIteration > regions[owner].Count {
			return false
		}
	}
	if workflowTerminal(r) {
		return r.Repeat == nil
	}
	owner := workflowRepeatOwner(regions, r.NodeID)
	if r.Repeat == nil {
		return owner == ""
	}
	frame := r.Repeat
	region, ok := regions[frame.NodeID]
	origin := workflowRepeatVisitNumber(r.ID, frame.VisitID)
	return ok && owner == frame.NodeID && frame.Count == region.Count && frame.Iteration >= 1 && frame.Iteration <= region.Count && origin > 0 && origin < r.VisitNumber && workflowRepeatActivation(r, frame.NodeID, frame.VisitID)
}
