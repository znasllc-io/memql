package calls

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func resourceDefinition() WorkflowDefinition {
	d := readinessDefinition()
	d.Resources = []WorkflowResourceSlot{{ID: "slides", Label: "Approved slides", Kind: "synthetic", Source: "screen_share"}}
	d.Nodes[0].Requirements = WorkflowRequirements{ResourceIDs: []string{"slides"}}
	return d
}
func TestWorkflowResourceSlotsSourceMappingRefsAndLegacyCompatibility(t *testing.T) {
	d := resourceDefinition()
	require.Empty(t, ValidateWorkflow(d))
	refs := map[string]WorkflowResourceRef{"slides": {ResourceID: "owner-source", Revision: 2}}
	require.True(t, ValidWorkflowResourceRefs(d, refs))
	require.False(t, ValidWorkflowResourceRefs(d, nil))
	require.False(t, ValidWorkflowResourceRefs(d, map[string]WorkflowResourceRef{"slides": {ResourceID: "owner-source", Revision: 0}}))
	refs["undeclared"] = WorkflowResourceRef{ResourceID: "owner-source", Revision: 2}
	require.False(t, ValidWorkflowResourceRefs(d, refs))
	for _, kind := range []string{"desktop", "window", "terminal", "synthetic"} {
		require.True(t, ValidWorkflowResourceSource(kind, "screen_share"))
		require.True(t, ValidWorkflowResourceSource(kind, "screen_share_audio"))
		require.False(t, ValidWorkflowResourceSource(kind, "camera"))
	}
	for _, kind := range []string{"camera", "microphone"} {
		require.True(t, ValidWorkflowResourceSource(kind, "camera"))
		require.True(t, ValidWorkflowResourceSource(kind, "microphone"))
		require.False(t, ValidWorkflowResourceSource(kind, "screen_share"))
	}
	require.True(t, ValidWorkflowResourceSource("", "camera"))
	require.False(t, ValidWorkflowResourceSource("ssh", "camera"))
	require.False(t, ValidWorkflowResourceSource("", "shell"))
	old := readinessDefinition()
	before, _ := json.Marshal(old)
	old.Resources = []WorkflowResourceSlot{}
	old.Nodes[0].Requirements.ResourceIDs = []string{}
	after, _ := json.Marshal(old)
	require.Equal(t, before, after)
}
func TestWorkflowResourceSlotsRejectUnknownDuplicateUnsupportedAndControllerRequirements(t *testing.T) {
	for name, change := range map[string]func(*WorkflowDefinition){
		"unknown-slot":          func(d *WorkflowDefinition) { d.Nodes[0].Requirements.ResourceIDs = []string{"other"} },
		"duplicate-requirement": func(d *WorkflowDefinition) { d.Nodes[0].Requirements.ResourceIDs = []string{"slides", "slides"} },
		"duplicate-slot":        func(d *WorkflowDefinition) { d.Resources = append(d.Resources, d.Resources[0]) },
		"unsupported-source":    func(d *WorkflowDefinition) { d.Resources[0].Source = "camera" },
		"too-many-slots":        func(d *WorkflowDefinition) { d.Resources = make([]WorkflowResourceSlot, 9) },
		"finish-requirement":    func(d *WorkflowDefinition) { d.Nodes[3].Requirements.ResourceIDs = []string{"slides"} },
	} {
		t.Run(name, func(t *testing.T) { d := resourceDefinition(); change(&d); require.NotEmpty(t, ValidateWorkflow(d)) })
	}
	require.True(t, EmptyWorkflowRequirements(WorkflowRequirements{}))
	require.False(t, EmptyWorkflowRequirements(WorkflowRequirements{ResourceIDs: []string{"slides"}}))
}
