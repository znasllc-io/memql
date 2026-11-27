package calls

import (
	"fmt"
	"slices"
	"time"
)

type WorkflowResourceSlot struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Kind   string `json:"kind,omitempty"`
	Source string `json:"source"`
}
type WorkflowResourceRef struct {
	ResourceID string `json:"resourceId"`
	Revision   int    `json:"revision"`
}
type WorkflowResourceBinding struct {
	WorkflowResourceRef
	LeaseID   string     `json:"leaseId,omitempty"`
	Identity  string     `json:"identity,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}
type WorkflowResourceOption struct {
	ResourceID string `json:"resourceId"`
	Revision   int    `json:"revision"`
	Label      string `json:"label"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
}

// Follow the actual managed ingress track-source mapping. This declaration
// observes an already authorized publisher; it does not expand its media grant.
func ValidWorkflowResourceSource(kind, source string) bool {
	if !slices.Contains([]string{"camera", "microphone", "screen_share", "screen_share_audio"}, source) {
		return false
	}
	if kind == "" {
		return true
	}
	switch kind {
	case "camera", "microphone":
		return source == "camera" || source == "microphone"
	case "desktop", "window", "terminal", "synthetic":
		return source == "screen_share" || source == "screen_share_audio"
	default:
		return false
	}
}
func EmptyWorkflowRequirements(r WorkflowRequirements) bool {
	return !r.Video && !r.ScreenShare && !r.Acknowledgment && r.SupervisorRoleID == "" && len(r.ResourceIDs) == 0
}
func workflowResourceIssues(d WorkflowDefinition) []WorkflowIssue {
	issues := []WorkflowIssue{}
	add := func(path, message string) {
		issues = append(issues, WorkflowIssue{Path: path, Code: "invalid_resource_requirement", Message: message})
	}
	if len(d.Resources) > 8 {
		add("resources", "Declare at most eight logical resource slots.")
	}
	slots := map[string]bool{}
	for i, slot := range d.Resources {
		if !ValidWorkflowID(slot.ID) || slots[slot.ID] || !workflowMetadataText(slot.Label, 120, true) || !ValidWorkflowResourceSource(slot.Kind, slot.Source) {
			add(fmt.Sprintf("resources.%d", i), "Resource slots need unique IDs, bounded labels and a supported publisher source for their optional kind.")
		}
		slots[slot.ID] = true
	}
	for _, node := range d.Nodes {
		ids := node.Requirements.ResourceIDs
		if len(ids) > 4 || len(ids) > 0 && slices.Contains([]string{"repeat", "parallel", "finish"}, node.Type) {
			add("nodes."+node.ID+".requirements.resourceIds", "Require at most four declared resources on a guidance step, never a controller or finish.")
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !slots[id] || seen[id] {
				add("nodes."+node.ID+".requirements.resourceIds", "Each resource requirement must name one distinct declared logical slot.")
			}
			seen[id] = true
		}
	}
	return issues
}

// Owner/grant authority, source revision, room and live lease checks belong to
// the native service. This pure shape check never authorizes a resource itself.
func ValidWorkflowResourceRefs(d WorkflowDefinition, refs map[string]WorkflowResourceRef) bool {
	if len(d.Resources) > 8 || len(refs) != len(d.Resources) {
		return false
	}
	for _, slot := range d.Resources {
		ref, ok := refs[slot.ID]
		if !ok || !ValidWorkflowID(slot.ID) || !ValidWorkflowID(ref.ResourceID) || ref.Revision < 1 || int64(ref.Revision) > 2147483647 {
			return false
		}
	}
	return true
}
