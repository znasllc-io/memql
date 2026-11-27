package calls

import (
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
)

type WorkflowReadinessSpec struct {
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Help           string `json:"help"`
	FallbackNodeID string `json:"fallbackNodeId,omitempty"`
}
type WorkflowReadinessWait struct {
	VisitID   string    `json:"visitId"`
	BlockedAt time.Time `json:"blockedAt"`
	Deadline  time.Time `json:"deadline"`
}

// Ordinary successors never include the separately reviewed timeout alternative.
// All graph edges remain visible to validation and worst-case planning.
func WorkflowOrdinaryNext(d WorkflowDefinition, id string) []string {
	next := WorkflowNext(d, id)
	node, _ := WorkflowNodeByID(d, id)
	if node.Readiness != nil && node.Readiness.FallbackNodeID != "" {
		next = slices.DeleteFunc(next, func(target string) bool { return target == node.Readiness.FallbackNodeID })
	}
	return next
}
func workflowReadinessIssues(d WorkflowDefinition) []WorkflowIssue {
	issues := []WorkflowIssue{}
	for _, node := range d.Nodes {
		r := node.Readiness
		if r == nil {
			continue
		}
		badHelp := strings.TrimSpace(r.Help) == "" || len(r.Help) > 1000 || strings.IndexFunc(r.Help, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0
		if slices.Contains([]string{"repeat", "parallel", "effect", "finish"}, node.Type) || !(node.Requirements.Video || node.Requirements.ScreenShare || node.Requirements.SupervisorRoleID != "" || len(node.Requirements.ResourceIDs) > 0) || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 3600 || badHelp {
			issues = append(issues, WorkflowIssue{Path: "nodes." + node.ID + ".readiness", Code: "invalid_readiness", Message: "Readiness needs a declared media/supervisor condition, useful help and a timeout of 1–3600 seconds on a guidance step."})
		}
		if r.FallbackNodeID != "" && (!ValidWorkflowID(r.FallbackNodeID) || r.FallbackNodeID == node.ID || !slices.Contains(WorkflowNext(d, node.ID), r.FallbackNodeID) || len(WorkflowOrdinaryNext(d, node.ID)) == 0) {
			issues = append(issues, WorkflowIssue{Path: "nodes." + node.ID + ".readiness", Code: "invalid_readiness_fallback", Message: "An approved fallback must be a distinct declared outgoing target alongside the ordinary path."})
		}
	}
	return issues
}
func ValidWorkflowFallbackRequest(r WorkflowRequest) bool {
	return r.Action == "fallback" && ValidWorkflowID(r.RunID) && r.ExpectedRevision > 0 && r.ExpectedEpoch > 0 && ValidWorkflowID(r.IdempotencyKey) && reflect.DeepEqual(r, WorkflowRequest{Action: "fallback", RunID: r.RunID, ExpectedRevision: r.ExpectedRevision, ExpectedEpoch: r.ExpectedEpoch, IdempotencyKey: r.IdempotencyKey, BranchID: r.BranchID, BranchVisitID: r.BranchVisitID})
}

type WorkflowRunExit struct {
	RunID   string    `json:"runId"`
	NodeID  string    `json:"nodeId"`
	VisitID string    `json:"visitId"`
	Outcome string    `json:"outcome"`
	Action  string    `json:"action"`
	At      time.Time `json:"at"`
}
