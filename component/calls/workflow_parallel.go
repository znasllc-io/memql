package calls

import "time"

func workflowParallelSpecs(d WorkflowDefinition) map[string]WorkflowParallelSpec {
	specs := map[string]WorkflowParallelSpec{}
	for _, n := range d.Nodes {
		if n.Parallel != nil {
			specs[n.ID] = *n.Parallel
		}
	}
	return specs
}

func WorkflowParallelRegions(d WorkflowDefinition) map[string]WorkflowParallelRegion {
	regions, issues := workflowBuildParallels(d, workflowParallelSpecs(d))
	if len(issues) != 0 {
		return nil
	}
	return regions
}

type WorkflowParallelSpec struct {
	Branches []WorkflowParallelBranchSpec `json:"branches"`
}
type WorkflowParallelBranchSpec struct {
	ID          string `json:"id"`
	EntryNodeID string `json:"entryNodeId"`
	UntilNodeID string `json:"untilNodeId"`
}
type WorkflowParallelProgress struct {
	ControllerVisitID string                   `json:"controllerVisitId"`
	Branches          []WorkflowParallelBranch `json:"branches"`
}
type WorkflowParallelBranch struct {
	ID                  string          `json:"id"`
	NodeID              string          `json:"nodeId"`
	VisitID             string          `json:"visitId"`
	Status              string          `json:"status"`
	AnchorAt            time.Time       `json:"anchorAt"`
	ElapsedSeconds      int64           `json:"elapsedSeconds"`
	DurationSeconds     int64           `json:"durationSeconds"`
	OvertimeSeconds     int64           `json:"overtimeSeconds"`
	AcknowledgedSeatIDs []string        `json:"acknowledgedSeatIds"`
	Blockers            []string        `json:"blockers"`
	AllowedActions      []string        `json:"allowedActions"`
	SeekableNodeIDs     []string        `json:"seekableNodeIds,omitempty"`
	History             []WorkflowVisit `json:"history"`
}
type WorkflowParallelRegion struct {
	ControllerID, AfterID string
	Branches              []WorkflowParallelBranchRegion
}
type WorkflowParallelBranchRegion struct {
	ID, EntryID, ExitID string
	Body                map[string]bool
}

// Parallelism is a bounded join-all region, never arbitrary concurrent effects.
// Branches are disjoint DAGs with one entry and one inclusive exit. No private
// branch acquires presentation/resource ownership or executes a media effect.
func workflowBuildParallels(d WorkflowDefinition, specs map[string]WorkflowParallelSpec) (map[string]WorkflowParallelRegion, []WorkflowIssue) {
	regions := map[string]WorkflowParallelRegion{}
	issues := []WorkflowIssue{}
	nodes := map[string]WorkflowNode{}
	next, parents := map[string][]string{}, map[string][]string{}
	for _, n := range d.Nodes {
		nodes[n.ID] = n
	}
	for _, e := range d.Edges {
		next[e.Source] = append(next[e.Source], e.Target)
		parents[e.Target] = append(parents[e.Target], e.Source)
	}
	owned := map[string]string{}
	for _, controller := range d.Nodes {
		spec, configured := specs[controller.ID]
		if controller.Type != "parallel" && !configured {
			continue
		}
		path := "nodes." + controller.ID + ".parallel"
		add := func(code, message string) {
			issues = append(issues, WorkflowIssue{Path: path, Code: code, Message: message})
		}
		if controller.Type != "parallel" || !configured || len(spec.Branches) < 2 || len(spec.Branches) > 4 || len(next[controller.ID]) != len(spec.Branches) {
			add("invalid_parallel", "A parallel controller needs two to four branches and exactly one edge to each branch entry.")
			continue
		}
		if controller.DurationSeconds != 0 || controller.RoleID != "" || controller.Requirements != (WorkflowRequirements{}) || controller.Visibility != "all" && controller.Visibility != "host" {
			add("invalid_parallel_controller", "Put timing, roles and readiness on branch steps, not the parallel controller.")
			continue
		}
		region := WorkflowParallelRegion{ControllerID: controller.ID}
		ids, entries, bodyOwner := map[string]bool{}, map[string]bool{}, map[string]string{}
		valid := true
		for _, branch := range spec.Branches {
			if !ValidWorkflowID(branch.ID) || ids[branch.ID] || !ValidWorkflowID(branch.EntryNodeID) || !ValidWorkflowID(branch.UntilNodeID) || entries[branch.EntryNodeID] {
				add("invalid_parallel_branch", "Branches need unique IDs and unique entries with valid inclusive exits.")
				valid = false
				continue
			}
			ids[branch.ID], entries[branch.EntryNodeID] = true, true
			body, color := map[string]bool{}, map[string]int{}
			var walk func(string) bool
			walk = func(id string) bool {
				if color[id] == 1 {
					return false
				}
				if color[id] == 2 {
					return true
				}
				n, exists := nodes[id]
				if !exists {
					return false
				}
				switch n.Type {
				case "instruction", "readiness", "turn", "discussion", "acknowledgment", "condition", "handoff":
				default:
					return false
				}
				body[id] = true
				if id == branch.UntilNodeID {
					color[id] = 2
					return true
				}
				if len(next[id]) == 0 {
					return false
				}
				color[id] = 1
				for _, target := range next[id] {
					if !walk(target) {
						return false
					}
				}
				color[id] = 2
				return true
			}
			if !walk(branch.EntryNodeID) || !body[branch.UntilNodeID] || len(next[branch.UntilNodeID]) != 1 || body[next[branch.UntilNodeID][0]] {
				add("invalid_parallel_region", "Each guidance branch must reach its one exit; nested repeat/parallel, presentation, finish and effect nodes are unavailable inside branches.")
				valid = false
				continue
			}
			after := next[branch.UntilNodeID][0]
			if region.AfterID != "" && region.AfterID != after {
				add("parallel_join_required", "Every branch exit must have one edge to the same join-all successor.")
				valid = false
			}
			region.AfterID = after
			for id := range body {
				if bodyOwner[id] != "" || owned[id] != "" {
					add("parallel_overlap", "Parallel branch bodies cannot overlap.")
					valid = false
				}
				bodyOwner[id] = branch.ID
				for _, parent := range parents[id] {
					if !body[parent] && !(id == branch.EntryNodeID && parent == controller.ID) {
						add("parallel_external_entry", "Enter a branch only through its controller and declared entry.")
						valid = false
					}
				}
			}
			region.Branches = append(region.Branches, WorkflowParallelBranchRegion{ID: branch.ID, EntryID: branch.EntryNodeID, ExitID: branch.UntilNodeID, Body: body})
		}
		for _, target := range next[controller.ID] {
			if !entries[target] {
				valid = false
				add("parallel_entry_edges", "Controller edges must exactly match the declared branch entries.")
			}
			delete(entries, target)
		}
		if len(entries) != 0 || bodyOwner[region.AfterID] != "" {
			valid = false
			add("parallel_join_required", "Every branch must enter once and exit to a common successor outside all branches.")
		}
		if valid {
			for id := range bodyOwner {
				owned[id] = controller.ID
			}
			regions[controller.ID] = region
		}
	}
	return regions, issues
}
