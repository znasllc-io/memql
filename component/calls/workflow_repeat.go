package calls

import "fmt"

// A repeat is a structured, finite region of the otherwise acyclic graph. The
// controller's sole edge enters the body; the named exit's sole edge leaves it.
// No graph edge points backward, and no template expansion owns runtime state.
type WorkflowRepeatSpec struct {
	Count       int    `json:"count"`
	UntilNodeID string `json:"untilNodeId"`
}
type WorkflowRepeatProgress struct {
	NodeID    string `json:"nodeId"`
	VisitID   string `json:"visitId"`
	Iteration int    `json:"iteration"`
	Count     int    `json:"count"`
}
type WorkflowRepeatRegion struct {
	ControllerID, EntryID, ExitID, AfterID string
	Count                                  int
	Body                                   map[string]bool
}

func workflowRepeatSpecs(d WorkflowDefinition) map[string]WorkflowRepeatSpec {
	specs := map[string]WorkflowRepeatSpec{}
	for _, node := range d.Nodes {
		if node.Repeat != nil {
			specs[node.ID] = *node.Repeat
		}
	}
	return specs
}

// WorkflowRepeatRegions exposes only validated structural regions. Runtime callers
// use a stored definition that has already passed the full graph validator.
func WorkflowRepeatRegions(d WorkflowDefinition) map[string]WorkflowRepeatRegion {
	regions, issues := workflowBuildRepeats(d, workflowRepeatSpecs(d))
	if len(issues) != 0 {
		return map[string]WorkflowRepeatRegion{}
	}
	return regions
}

// The caller first validates the ordinary DAG. Keeping the region check pure
// also lets authoring validate structure without creating a run or a provider.
func workflowBuildRepeats(d WorkflowDefinition, specs map[string]WorkflowRepeatSpec) (map[string]WorkflowRepeatRegion, []WorkflowIssue) {
	regions := map[string]WorkflowRepeatRegion{}
	issues := []WorkflowIssue{}
	nodes := map[string]WorkflowNode{}
	next, parents := map[string][]string{}, map[string][]string{}
	for _, node := range d.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range d.Edges {
		next[edge.Source] = append(next[edge.Source], edge.Target)
		parents[edge.Target] = append(parents[edge.Target], edge.Source)
	}
	owner := map[string]string{}
	for _, node := range d.Nodes {
		spec, configured := specs[node.ID]
		if node.Type != "repeat" && !configured {
			continue
		}
		path := "nodes." + node.ID + ".repeat"
		add := func(code, text string) { issues = append(issues, WorkflowIssue{Path: path, Code: code, Message: text}) }
		if node.Type != "repeat" || !configured || spec.Count < 1 || spec.Count > 8 || !ValidWorkflowID(spec.UntilNodeID) || spec.UntilNodeID == node.ID || len(next[node.ID]) != 1 {
			add("invalid_repeat", "A repeat needs 1–8 rounds, one body entry and a distinct body exit.")
			continue
		}
		if node.DurationSeconds != 0 || node.RoleID != "" || node.Requirements != (WorkflowRequirements{}) || node.Visibility != "all" && node.Visibility != "host" {
			add("invalid_repeat_controller", "Put timing, roles and readiness on the body steps, not the repeat controller.")
			continue
		}
		body, color := map[string]bool{}, map[string]int{}
		var walk func(string) bool
		walk = func(id string) bool {
			if color[id] == 1 {
				return false
			}
			if color[id] == 2 {
				return true
			}
			member, exists := nodes[id]
			if !exists || member.Type == "repeat" || member.Type == "parallel" || member.Type == "finish" {
				return false
			}
			body[id] = true
			if id == spec.UntilNodeID {
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
		entry := next[node.ID][0]
		if !walk(entry) || !body[spec.UntilNodeID] || len(next[spec.UntilNodeID]) != 1 || body[next[spec.UntilNodeID][0]] {
			add("invalid_repeat_region", "Every body path must reach its one exit before leaving. Nested repeats, parallel regions, finish steps and cycles are unavailable inside a repeat.")
			continue
		}
		valid := true
		for id := range body {
			if other := owner[id]; other != "" {
				add("overlapping_repeat", "Repeat bodies cannot overlap.")
				valid = false
			}
			for _, parent := range parents[id] {
				if !body[parent] && !(id == entry && parent == node.ID) {
					add("repeat_external_entry", "Enter the repeat body only through its controller.")
					valid = false
				}
			}
		}
		if !valid {
			continue
		}
		for id := range body {
			owner[id] = node.ID
		}
		regions[node.ID] = WorkflowRepeatRegion{ControllerID: node.ID, EntryID: entry, ExitID: spec.UntilNodeID, AfterID: next[spec.UntilNodeID][0], Count: spec.Count, Body: body}
	}
	return regions, issues
}

type workflowPlannedRoute struct {
	seconds int64
	visits  int
	nodes   []string
}

// Timing and visit-count bounds are independently maximized: the longest timed
// path need not have the most steps. The displayed route uses the time maximum.
func workflowPlan(d WorkflowDefinition, repeats map[string]WorkflowRepeatRegion) workflowPlannedRoute {
	parallels := WorkflowParallelRegions(d)
	nodes, next := map[string]WorkflowNode{}, map[string][]string{}
	for _, node := range d.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range d.Edges {
		next[edge.Source] = append(next[edge.Source], edge.Target)
	}
	type key struct{ node, stop string }
	memo := map[key]workflowPlannedRoute{}
	var route func(string, string) workflowPlannedRoute
	route = func(id, stop string) workflowPlannedRoute {
		k := key{id, stop}
		if found, ok := memo[k]; ok {
			return found
		}
		node := nodes[id]
		result := workflowPlannedRoute{seconds: node.DurationSeconds, visits: 1, nodes: []string{id}}
		if node.Type == "finish" {
			result.seconds = 0
		} else if id == stop {
			// The inclusive body exit contributes its own allowance exactly once.
		} else if repeat, ok := repeats[id]; ok {
			body, after := route(repeat.EntryID, repeat.ExitID), route(repeat.AfterID, stop)
			result.seconds += body.seconds*int64(repeat.Count) + after.seconds
			result.visits += body.visits*repeat.Count + after.visits
			for range repeat.Count {
				result.nodes = append(result.nodes, body.nodes...)
			}
			result.nodes = append(result.nodes, after.nodes...)
		} else if parallel, ok := parallels[id]; ok {
			seconds := int64(0)
			for _, branch := range parallel.Branches {
				body := route(branch.EntryID, branch.ExitID)
				seconds = max(seconds, body.seconds)
				result.visits += body.visits
				result.nodes = append(result.nodes, body.nodes...)
			}
			after := route(parallel.AfterID, stop)
			result.seconds += seconds + after.seconds
			result.visits += after.visits
			result.nodes = append(result.nodes, after.nodes...)
		} else {
			var best workflowPlannedRoute
			chosen, visits := "", 0
			for _, target := range next[id] {
				candidate := route(target, stop)
				if candidate.visits > visits {
					visits = candidate.visits
				}
				if chosen == "" || candidate.seconds > best.seconds || candidate.seconds == best.seconds && target < chosen {
					best, chosen = candidate, target
				}
			}
			result.seconds += best.seconds
			result.visits += visits
			result.nodes = append(result.nodes, best.nodes...)
		}
		memo[k] = result
		return result
	}
	return route(d.EntryNodeID, "")
}

func workflowRepeatBounds(d WorkflowDefinition, repeats map[string]WorkflowRepeatRegion) []WorkflowIssue {
	if len(repeats) == 0 && len(WorkflowParallelRegions(d)) == 0 {
		return nil
	}
	planned := workflowPlan(d, repeats)
	if planned.visits > 256 || planned.seconds > 86400 {
		return []WorkflowIssue{{Path: "nodes", Code: "repeat_plan_limit", Message: fmt.Sprintf("The expanded plan has %d visits and %d seconds; structured workflows allow at most 256 planned visits and 24 hours.", planned.visits, planned.seconds)}}
	}
	return nil
}
