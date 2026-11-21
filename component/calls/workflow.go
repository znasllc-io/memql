package calls

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// WorkflowDefinition is declarative session guidance, never executable server code.
// A published version is pinned into each run; visual positions have no authority.
type WorkflowDefinition struct {
	ID               string         `json:"id"`
	Version          int            `json:"version"`
	Revision         int64          `json:"revision"`
	Status           string         `json:"status"`
	Title            string         `json:"title"`
	Description      string         `json:"description"`
	Instructions     string         `json:"instructions"`
	RequiresInsights bool           `json:"requiresInsights"`
	Roles            []WorkflowRole `json:"roles"`
	Nodes            []WorkflowNode `json:"nodes"`
	Edges            []WorkflowEdge `json:"edges"`
	EntryNodeID      string         `json:"entryNodeId"`
}
type WorkflowRole struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Required bool   `json:"required,omitempty"`
}
type WorkflowPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}
type WorkflowRequirements struct {
	Video            bool   `json:"video,omitempty"`
	ScreenShare      bool   `json:"screenShare,omitempty"`
	SupervisorRoleID string `json:"supervisorRoleId,omitempty"`
	Acknowledgment   bool   `json:"acknowledgment,omitempty"`
}
type WorkflowNode struct {
	ID              string               `json:"id"`
	Type            string               `json:"type"`
	Label           string               `json:"label"`
	Instructions    string               `json:"instructions,omitempty"`
	Visibility      string               `json:"visibility"`
	RoleID          string               `json:"roleId,omitempty"`
	DurationSeconds int64                `json:"durationSeconds,omitempty"`
	Requirements    WorkflowRequirements `json:"requirements,omitempty"`
	Position        *WorkflowPosition    `json:"position,omitempty"`
}
type WorkflowEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Label  string `json:"label,omitempty"`
}
type WorkflowIssue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type WorkflowSimulation struct {
	NodeIDs        []string `json:"nodeIds"`
	MaximumSeconds int64    `json:"maximumSeconds"`
	Notes          []string `json:"notes"`
}
type WorkflowBinding struct {
	BookingID string            `json:"bookingId"`
	SkillID   string            `json:"skillId"`
	Version   int               `json:"version"`
	Revision  int64             `json:"revision"`
	Bindings  map[string]string `json:"bindings"`
}

type WorkflowRequest struct {
	BookingID        string              `json:"bookingId,omitempty"`
	Action           string              `json:"action"`
	Definition       *WorkflowDefinition `json:"definition,omitempty"`
	SkillID          string              `json:"skillId,omitempty"`
	Version          int                 `json:"version,omitempty"`
	RunID            string              `json:"runId,omitempty"`
	ExpectedRevision int64               `json:"expectedRevision,omitempty"`
	ExpectedEpoch    int64               `json:"expectedEpoch,omitempty"`
	IdempotencyKey   string              `json:"idempotencyKey,omitempty"`
	NodeID           string              `json:"nodeId,omitempty"`
	Seconds          int64               `json:"seconds,omitempty"`
	Bindings         map[string]string   `json:"bindings,omitempty"`
	Acknowledged     bool                `json:"acknowledged,omitempty"`
}
type WorkflowEvent struct {
	ID         string    `json:"id"`
	Sequence   int64     `json:"sequence"`
	At         time.Time `json:"at"`
	Type       string    `json:"type"`
	NodeID     string    `json:"nodeId"`
	VisitID    string    `json:"visitId"`
	Message    string    `json:"message"`
	Visibility string    `json:"visibility"`
	RoleID     string    `json:"roleId,omitempty"`
}
type WorkflowVisit struct {
	NodeID         string `json:"nodeId"`
	VisitID        string `json:"visitId"`
	ElapsedSeconds int64  `json:"elapsedSeconds"`
}
type WorkflowRun struct {
	ID                  string             `json:"id"`
	RoomID              string             `json:"roomId"`
	DefinitionID        string             `json:"definitionId"`
	DefinitionVersion   int                `json:"definitionVersion"`
	Definition          WorkflowDefinition `json:"definition"`
	Revision            int64              `json:"revision"`
	ControlEpoch        int64              `json:"controlEpoch"`
	Status              string             `json:"status"`
	NodeID              string             `json:"nodeId"`
	VisitID             string             `json:"visitId"`
	VisitNumber         int64              `json:"visitNumber"`
	StartedAt           time.Time          `json:"startedAt"`
	AnchorAt            time.Time          `json:"anchorAt"`
	ElapsedSeconds      int64              `json:"elapsedSeconds"`
	DurationSeconds     int64              `json:"durationSeconds"`
	OvertimeSeconds     int64              `json:"overtimeSeconds"`
	Bindings            map[string]string  `json:"bindings"`
	AcknowledgedSeatIDs []string           `json:"acknowledgedSeatIds"`
	Hold                string             `json:"hold"`
	Blockers            []string           `json:"blockers"`
	AllowedActions      []string           `json:"allowedActions"`
	Events              []WorkflowEvent    `json:"events"`
	History             []WorkflowVisit    `json:"history"`
	ServerNow           time.Time          `json:"serverNow"`
}
type WorkflowResult struct {
	Binding     *WorkflowBinding     `json:"binding,omitempty"`
	Catalog     []WorkflowDefinition `json:"catalog,omitempty"`
	Definitions []WorkflowDefinition `json:"definitions,omitempty"`
	Definition  *WorkflowDefinition  `json:"definition,omitempty"`
	Issues      []WorkflowIssue      `json:"issues,omitempty"`
	Simulation  *WorkflowSimulation  `json:"simulation,omitempty"`
	Run         *WorkflowRun         `json:"run,omitempty"`
	ServerNow   time.Time            `json:"serverNow"`
}

var workflowID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$`)

func ValidWorkflowID(v string) bool { return workflowID.MatchString(v) }

// ValidateWorkflow admits a finite DAG of supported, bounded primitives. Unsupported
// analysis/effects/loops are refusals, not a promise of unimplemented automation.
func ValidateWorkflow(d WorkflowDefinition) []WorkflowIssue {
	issues := []WorkflowIssue{}
	add := func(path, code, msg string) { issues = append(issues, WorkflowIssue{path, code, msg}) }
	if !ValidWorkflowID(d.ID) {
		add("id", "invalid_id", "Use a bounded stable skill identifier.")
	}
	if strings.TrimSpace(d.Title) == "" || len(d.Title) > 120 {
		add("title", "invalid_title", "A title of at most 120 bytes is required.")
	}
	if !d.RequiresInsights {
		add("requiresInsights", "insights_required", "Skills always require Insights.")
	}
	if len(d.Description) > 4000 || len(d.Instructions) > 16000 {
		add("instructions", "too_large", "Instructions exceed their bound.")
	}
	if len(d.Roles) > 16 || len(d.Nodes) == 0 || len(d.Nodes) > 64 || len(d.Edges) > 128 {
		add("nodes", "graph_limit", "Use 1–64 nodes, at most 16 roles and 128 edges.")
		return issues
	}
	raw, _ := json.Marshal(d)
	if len(raw) > 128<<10 {
		add("definition", "too_large", "The definition exceeds 128 KiB.")
	}
	roles := map[string]bool{}
	for i, r := range d.Roles {
		if !ValidWorkflowID(r.ID) || roles[r.ID] || r.Label == "" || len(r.Label) > 120 {
			add(fmt.Sprintf("roles.%d", i), "invalid_role", "Roles need unique IDs and bounded labels.")
		}
		roles[r.ID] = true
	}
	nodes := map[string]WorkflowNode{}
	for i, n := range d.Nodes {
		path := fmt.Sprintf("nodes.%d", i)
		if !ValidWorkflowID(n.ID) {
			add(path, "invalid_id", "Node identifier is invalid.")
		}
		if _, ok := nodes[n.ID]; ok {
			add(path, "duplicate_node", "Node identifiers must be unique.")
		}
		nodes[n.ID] = n
		switch n.Type {
		case "readiness", "instruction", "turn", "discussion", "presentation", "acknowledgment", "condition", "handoff", "finish":
		default:
			add(path, "unsupported_node", "This node type has no implemented runtime capability.")
		}
		if n.Label == "" || len(n.Label) > 120 || len(n.Instructions) > 8000 {
			add(path, "invalid_content", "Node label/instructions exceed their bounds.")
		}
		if n.Visibility != "all" && n.Visibility != "host" && n.Visibility != "role" {
			add(path, "invalid_visibility", "Visibility must be all, host or role.")
		}
		if n.RoleID != "" && !roles[n.RoleID] || n.Visibility == "role" && n.RoleID == "" {
			add(path, "unknown_role", "The node must reference a declared role.")
		}
		if n.Requirements.SupervisorRoleID != "" && !roles[n.Requirements.SupervisorRoleID] {
			add(path, "unknown_supervisor", "Declare the supervisor role first.")
		}
		if n.DurationSeconds < 0 || n.DurationSeconds > 14400 {
			add(path, "invalid_duration", "Step time must be between zero and four hours.")
		}
	}
	if _, ok := nodes[d.EntryNodeID]; !ok {
		add("entryNodeId", "missing_entry", "Choose an existing entry node.")
	}
	edges := map[string]bool{}
	out := map[string][]string{}
	ins := map[string]int{}
	for i, e := range d.Edges {
		path := fmt.Sprintf("edges.%d", i)
		if !ValidWorkflowID(e.ID) || edges[e.ID] {
			add(path, "duplicate_edge", "Edges need unique bounded IDs.")
		}
		edges[e.ID] = true
		_, a := nodes[e.Source]
		_, b := nodes[e.Target]
		if !a || !b {
			add(path, "missing_node", "Edge endpoints must exist.")
		}
		out[e.Source] = append(out[e.Source], e.Target)
		ins[e.Target]++
	}
	for id, n := range nodes {
		if n.Type == "finish" && len(out[id]) != 0 {
			add("nodes."+id, "finish_has_edges", "Finish nodes have no outgoing edges.")
		}
		if n.Type != "finish" && len(out[id]) == 0 {
			add("nodes."+id, "missing_finish", "Every path must reach a finish node.")
		}
		if n.Type != "condition" && len(out[id]) > 1 {
			add("nodes."+id, "ambiguous_branch", "Multiple successors require an explicit condition node.")
		}
	}
	color := map[string]int{}
	var visit func(string)
	visit = func(id string) {
		if color[id] == 1 {
			add("edges", "cycle", "Loops require a bounded runtime capability and are not supported.")
			return
		}
		if color[id] == 2 {
			return
		}
		color[id] = 1
		for _, next := range out[id] {
			visit(next)
		}
		color[id] = 2
	}
	visit(d.EntryNodeID)
	for id := range nodes {
		if color[id] == 0 {
			add("nodes."+id, "unreachable", "All nodes must be reachable from the entry.")
		}
	}
	return issues
}
func WorkflowNodeByID(d WorkflowDefinition, id string) (WorkflowNode, bool) {
	for _, n := range d.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return WorkflowNode{}, false
}
func WorkflowNext(d WorkflowDefinition, id string) []string {
	out := []string{}
	for _, e := range d.Edges {
		if e.Source == id {
			out = append(out, e.Target)
		}
	}
	return out
}
func SimulateWorkflow(d WorkflowDefinition) WorkflowSimulation {
	out := WorkflowSimulation{NodeIDs: []string{}, Notes: []string{"Dry run only; no media actions, charges or participant acknowledgments are performed.", "Timers are advisory. Pausing guidance does not pause call billing."}}
	for _, n := range d.Nodes {
		out.NodeIDs = append(out.NodeIDs, n.ID)
		out.MaximumSeconds += n.DurationSeconds
	}
	return out
}
func WorkflowElapsed(r WorkflowRun, now time.Time) int64 {
	v := r.ElapsedSeconds
	if r.Status == "running" && !r.AnchorAt.IsZero() && now.After(r.AnchorAt) {
		v += int64(now.Sub(r.AnchorAt) / time.Second)
	}
	return v
}
