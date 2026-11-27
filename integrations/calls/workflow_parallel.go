package calls

import (
	"context"
	"fmt"
	"slices"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
)

func workflowParallelOwner(d callrules.WorkflowDefinition, node string) string {
	for controller, region := range callrules.WorkflowParallelRegions(d) {
		for _, branch := range region.Branches {
			if branch.Body[node] {
				return controller
			}
		}
	}
	return ""
}
func workflowParallelRegion(r *callrules.WorkflowRun, id string) (callrules.WorkflowParallelBranchRegion, bool) {
	region, ok := callrules.WorkflowParallelRegions(r.Definition)[r.NodeID]
	if ok && r.Parallel != nil {
		for _, branch := range region.Branches {
			if branch.ID == id {
				return branch, true
			}
		}
	}
	return callrules.WorkflowParallelBranchRegion{}, false
}
func workflowParallelJoined(r *callrules.WorkflowRun) bool {
	if r.Parallel == nil {
		return false
	}
	for _, b := range r.Parallel.Branches {
		if b.Status != "joined" {
			return false
		}
	}
	return true
}
func workflowBranchElapsed(r *callrules.WorkflowRun, b callrules.WorkflowParallelBranch, now time.Time) int64 {
	seconds := b.ElapsedSeconds
	if r.Status == "running" && b.Status == "running" && !b.AnchorAt.IsZero() && now.After(b.AnchorAt) {
		seconds += int64(now.Sub(b.AnchorAt) / time.Second)
	}
	return seconds
}
func workflowParallelFreeze(r *callrules.WorkflowRun, now time.Time) {
	if r.Parallel != nil {
		for i := range r.Parallel.Branches {
			b := &r.Parallel.Branches[i]
			b.ElapsedSeconds = workflowBranchElapsed(r, *b, now)
			b.AnchorAt = time.Time{}
		}
	}
}
func workflowParallelResume(r *callrules.WorkflowRun, now time.Time) {
	if r.Parallel != nil {
		for i := range r.Parallel.Branches {
			b := &r.Parallel.Branches[i]
			if b.Status != "joined" && len(b.Blockers) == 0 {
				if b.Status == "running" && !b.AnchorAt.IsZero() {
					continue
				}
				b.Status, b.AnchorAt = "running", now
			}
		}
	}
}
func workflowAppendVisit(r *callrules.WorkflowRun, visit callrules.WorkflowVisit) {
	r.History = append(r.History, visit)
	if len(r.History) > 256 {
		r.History = r.History[len(r.History)-256:]
	}
}
func workflowParallelArchive(r *callrules.WorkflowRun, now time.Time) {
	if r.Parallel == nil {
		return
	}
	for _, b := range r.Parallel.Branches {
		if b.Status != "joined" {
			workflowAppendVisit(r, callrules.WorkflowVisit{NodeID: b.NodeID, VisitID: b.VisitID, ElapsedSeconds: workflowBranchElapsed(r, b, now), ParallelVisitID: r.Parallel.ControllerVisitID, BranchID: b.ID})
		}
	}
	r.Parallel = nil
}
func workflowParallelEnter(r *callrules.WorkflowRun, now time.Time) {
	region, ok := callrules.WorkflowParallelRegions(r.Definition)[r.NodeID]
	if !ok {
		return
	}
	r.Parallel = &callrules.WorkflowParallelProgress{ControllerVisitID: r.VisitID, Branches: []callrules.WorkflowParallelBranch{}}
	for _, entry := range region.Branches {
		n, _ := callrules.WorkflowNodeByID(r.Definition, entry.EntryID)
		r.VisitNumber++
		b := callrules.WorkflowParallelBranch{ID: entry.ID, NodeID: n.ID, VisitID: fmt.Sprintf("%s-v%d", r.ID, r.VisitNumber), Status: "running", AnchorAt: now, DurationSeconds: n.DurationSeconds, AcknowledgedSeatIDs: []string{}, Blockers: []string{}, AllowedActions: []string{}, History: []callrules.WorkflowVisit{}}
		r.Parallel.Branches = append(r.Parallel.Branches, b)
		workflowBranchEvent(r, b, "branch_entered", n.Label, now)
	}
}
func workflowBranchEvent(r *callrules.WorkflowRun, b callrules.WorkflowParallelBranch, kind, message string, now time.Time) {
	outerNode, outerVisit := r.NodeID, r.VisitID
	r.NodeID, r.VisitID = b.NodeID, b.VisitID
	workflowEvent(r, kind, message, now)
	r.NodeID, r.VisitID = outerNode, outerVisit
	e := &r.Events[len(r.Events)-1]
	e.ParallelVisitID, e.BranchID = r.Parallel.ControllerVisitID, b.ID
}

// The isolated view reuses the ordinary role/readiness/navigation projection.
// Its graph and history contain only this activation's branch, so ordinary
// previous/seek can never escape to a sibling or a prior parallel activation.
func workflowBranchView(r *callrules.WorkflowRun, b callrules.WorkflowParallelBranch) callrules.WorkflowRun {
	v := *r
	v.Parallel, v.Repeat = nil, nil
	v.NodeID, v.VisitID = b.NodeID, b.VisitID
	v.AnchorAt, v.ElapsedSeconds, v.DurationSeconds = b.AnchorAt, b.ElapsedSeconds, b.DurationSeconds
	v.AcknowledgedSeatIDs, v.Blockers = slices.Clone(b.AcknowledgedSeatIDs), slices.Clone(b.Blockers)
	v.History = slices.Clone(b.History)
	for i := range v.History {
		v.History[i].ParallelVisitID, v.History[i].BranchID = "", ""
	}
	if r.Status == "running" || b.Status == "blocked" {
		v.Status = b.Status
	}
	region, _ := workflowParallelRegion(r, b.ID)
	v.Definition.Nodes, v.Definition.Edges = nil, nil
	for _, n := range r.Definition.Nodes {
		if region.Body[n.ID] {
			v.Definition.Nodes = append(v.Definition.Nodes, n)
		}
	}
	for _, e := range r.Definition.Edges {
		if region.Body[e.Source] && region.Body[e.Target] {
			v.Definition.Edges = append(v.Definition.Edges, e)
		}
	}
	return v
}
func (s *Service) workflowParallelObserve(ctx context.Context, site callrules.SiteRef, room roomRow, r *callrules.WorkflowRun, now time.Time) (bool, error) {
	if r.Parallel == nil || workflowTerminal(r) {
		return false, nil
	}
	changed := false
	for i := range r.Parallel.Branches {
		b := &r.Parallel.Branches[i]
		if b.Status == "joined" {
			continue
		}
		blockers, err := s.workflowRequirements(ctx, site, room, *r, b.NodeID)
		if err != nil {
			return false, err
		}
		if len(blockers) > 0 && (b.Status != "blocked" || !slices.Equal(b.Blockers, blockers)) {
			b.ElapsedSeconds = workflowBranchElapsed(r, *b, now)
			b.AnchorAt = time.Time{}
			b.Status, b.Blockers = "blocked", blockers
			workflowBranchEvent(r, *b, "branch_blocked", "This branch needs its assigned participant or required media.", now)
			changed = true
		} else if len(blockers) == 0 && len(b.Blockers) > 0 {
			b.Blockers = []string{}
			workflowBranchEvent(r, *b, "branch_requirements_restored", "Requirements restored. Explicit resume is still required.", now)
			changed = true
		}
	}
	if changed {
		r.Revision++
		r.ControlEpoch++
	}
	return changed, nil
}
func (s *Service) workflowParallelCommand(ctx context.Context, site callrules.SiteRef, room roomRow, r *callrules.WorkflowRun, seat seatRow, agent bool, req callrules.WorkflowRequest, now time.Time) error {
	if r.Parallel == nil || !slices.Contains([]string{"next", "previous", "seek", "extend", "ack", "end_turn"}, req.Action) {
		return callrules.ErrInvalid
	}
	index := slices.IndexFunc(r.Parallel.Branches, func(b callrules.WorkflowParallelBranch) bool { return b.ID == req.BranchID })
	if index < 0 || r.Parallel.Branches[index].VisitID != req.BranchVisitID {
		return accountRefusal("workflow_branch_visit_conflict")
	}
	b := &r.Parallel.Branches[index]
	if b.Status == "joined" {
		return accountRefusal("workflow_branch_joined")
	}
	v := workflowBranchView(r, *b)
	node, _ := callrules.WorkflowNodeByID(v.Definition, v.NodeID)
	if !seat.Host && (node.RoleID == "" || r.Bindings[node.RoleID] != seat.ID || req.Action != "ack" && (req.Action != "end_turn" || node.Type != "turn")) {
		return notHost()
	}
	if !seat.Host && req.Action == "end_turn" && v.Status != "running" {
		return accountRefusal("workflow_paused")
	}
	if !agent && seat.Host && slices.Contains([]string{"next", "previous", "seek", "extend"}, req.Action) {
		r.ControlEpoch++
		r.Hold = "human"
	}
	switch req.Action {
	case "ack":
		if !req.Acknowledged || node.Type != "acknowledgment" && !node.Requirements.Acknowledgment {
			return callrules.ErrInvalid
		}
		if node.RoleID != "" && r.Bindings[node.RoleID] != seat.ID {
			return accountRefusal("workflow_acknowledgment_actor")
		}
		if !slices.Contains(b.AcknowledgedSeatIDs, seat.ID) {
			b.AcknowledgedSeatIDs = append(b.AcknowledgedSeatIDs, seat.ID)
			workflowBranchEvent(r, *b, "acknowledged", "The assigned participant acknowledged this branch step.", now)
		}
	case "extend":
		if req.Seconds < 1 || req.Seconds > 3600 || b.DurationSeconds+req.Seconds > 14400 {
			return callrules.ErrInvalid
		}
		b.DurationSeconds += req.Seconds
		workflowBranchEvent(r, *b, "extended", "The host extended this branch's advisory time.", now)
	default:
		if err := s.workflowEntitlement(ctx, site, room); err != nil {
			return err
		}
		if !workflowAckReady(&v) {
			return accountRefusal("workflow_acknowledgment_required")
		}
		if r.Status == "blocked" || b.Status == "blocked" {
			return accountRefusal("workflow_requirements_unavailable")
		}
		region, _ := workflowParallelRegion(r, b.ID)
		target := req.NodeID
		join := false
		switch req.Action {
		case "previous":
			target = workflowPreviousNode(&v)
			if target == "" {
				return accountRefusal("workflow_no_previous")
			}
		case "seek":
			if !workflowCanSeek(&v, target) {
				return accountRefusal("workflow_seek_requires_visited_step")
			}
		default:
			if b.NodeID == region.ExitID {
				if target != "" {
					return callrules.ErrInvalid
				}
				join = true
			} else {
				next := callrules.WorkflowNext(v.Definition, b.NodeID)
				if target == "" && len(next) == 1 {
					target = next[0]
				}
				if !slices.Contains(next, target) {
					return accountRefusal("workflow_branch_required")
				}
			}
		}
		if !join {
			if !region.Body[target] {
				return accountRefusal("workflow_branch_required")
			}
			blockers, err := s.workflowRequirements(ctx, site, room, *r, target)
			if err != nil {
				return err
			}
			if len(blockers) != 0 {
				return accountRefusal("workflow_requirements_unavailable")
			}
		}
		visit := callrules.WorkflowVisit{NodeID: b.NodeID, VisitID: b.VisitID, ElapsedSeconds: workflowBranchElapsed(r, *b, now), ParallelVisitID: r.Parallel.ControllerVisitID, BranchID: b.ID}
		b.History = append(b.History, visit)
		if len(b.History) > 256 {
			b.History = b.History[len(b.History)-256:]
		}
		workflowAppendVisit(r, visit)
		if join {
			b.ElapsedSeconds, b.AnchorAt, b.Status = visit.ElapsedSeconds, time.Time{}, "joined"
			workflowBranchEvent(r, *b, "branch_joined", "This branch is waiting for the join-all barrier.", now)
		} else {
			n, _ := callrules.WorkflowNodeByID(r.Definition, target)
			r.VisitNumber++
			b.NodeID, b.VisitID = target, fmt.Sprintf("%s-v%d", r.ID, r.VisitNumber)
			b.ElapsedSeconds, b.DurationSeconds, b.AnchorAt = 0, n.DurationSeconds, now
			if r.Status != "running" {
				b.AnchorAt = time.Time{}
			}
			b.AcknowledgedSeatIDs, b.Blockers = []string{}, []string{}
			workflowBranchEvent(r, *b, "step_entered", n.Label, now)
		}
	}
	return nil
}

func workflowParallelProjection(r *callrules.WorkflowRun, run callrules.WorkflowRun, seat seatRow, agent bool, now time.Time) {
	if run.Parallel == nil {
		return
	}
	p := &callrules.WorkflowParallelProgress{ControllerVisitID: run.Parallel.ControllerVisitID, Branches: slices.Clone(run.Parallel.Branches)}
	r.Parallel = p
	for i := range p.Branches {
		b := &p.Branches[i]
		b.AllowedActions, b.SeekableNodeIDs = []string{}, []string{}
		b.History, b.Blockers, b.AcknowledgedSeatIDs = slices.Clone(b.History), slices.Clone(b.Blockers), slices.Clone(b.AcknowledgedSeatIDs)
		b.ElapsedSeconds = workflowBranchElapsed(&run, *b, now)
		b.OvertimeSeconds = 0
		if b.DurationSeconds > 0 {
			b.OvertimeSeconds = max(0, b.ElapsedSeconds-b.DurationSeconds)
		}
		if b.Status == "joined" || workflowTerminal(&run) {
			continue
		}
		view := workflowBranchView(&run, run.Parallel.Branches[i])
		projected := workflowProjection(view, seat, agent, now)
		b.AllowedActions = slices.DeleteFunc(projected.AllowedActions, func(action string) bool {
			return !slices.Contains([]string{"next", "previous", "seek", "extend", "ack", "end_turn"}, action)
		})
		b.SeekableNodeIDs = projected.SeekableNodeIDs
	}
}

func workflowValidParallel(v *workflowState) bool {
	if v.Kind != "run" || v.Run == nil {
		return true
	}
	r := v.Run
	regions := callrules.WorkflowParallelRegions(r.Definition)
	for _, visit := range r.History {
		owner := workflowParallelOwner(r.Definition, visit.NodeID)
		if owner == "" {
			if visit.ParallelVisitID != "" || visit.BranchID != "" {
				return false
			}
		} else if !workflowParallelVisitValid(r, regions[owner], visit) {
			return false
		}
	}
	region, controller := regions[r.NodeID]
	if r.Parallel == nil {
		return workflowParallelOwner(r.Definition, r.NodeID) == "" && (!controller || workflowTerminal(r))
	}
	if !controller || r.Repeat != nil || r.Parallel.ControllerVisitID != r.VisitID || len(r.Parallel.Branches) != len(region.Branches) {
		return false
	}
	seen := map[string]bool{r.VisitID: true}
	for i, b := range r.Parallel.Branches {
		definition := region.Branches[i]
		number := workflowRepeatVisitNumber(r.ID, b.VisitID)
		if b.ID != definition.ID || !definition.Body[b.NodeID] || seen[b.VisitID] || number <= workflowRepeatVisitNumber(r.ID, r.VisitID) || number > r.VisitNumber || !slices.Contains([]string{"running", "blocked", "joined"}, b.Status) || b.ElapsedSeconds < 0 || b.DurationSeconds < 0 || b.DurationSeconds > 14400 || len(b.History) > 256 || len(b.AcknowledgedSeatIDs) > 100 || len(b.Blockers) > 16 || (b.Status == "running" && r.Status == "running" && b.AnchorAt.IsZero()) || (b.Status == "joined" && b.NodeID != definition.ExitID) {
			return false
		}
		seen[b.VisitID] = true
		for _, visit := range b.History {
			if visit.ParallelVisitID != r.VisitID || visit.BranchID != b.ID || !workflowParallelVisitValid(r, region, visit) {
				return false
			}
		}
	}
	return true
}
func workflowParallelVisitValid(r *callrules.WorkflowRun, region callrules.WorkflowParallelRegion, visit callrules.WorkflowVisit) bool {
	activation := workflowRepeatVisitNumber(r.ID, visit.ParallelVisitID)
	number := workflowRepeatVisitNumber(r.ID, visit.VisitID)
	if activation < 1 || number <= activation || number > r.VisitNumber || visit.RepeatVisitID != "" || visit.RepeatIteration != 0 {
		return false
	}
	for _, branch := range region.Branches {
		if branch.ID == visit.BranchID && branch.Body[visit.NodeID] {
			if visit.ParallelVisitID == r.VisitID && r.NodeID == region.ControllerID {
				return true
			}
			return workflowRepeatActivation(r, region.ControllerID, visit.ParallelVisitID)
		}
	}
	return false
}
