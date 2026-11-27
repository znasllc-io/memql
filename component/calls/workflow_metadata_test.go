package calls

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func metadataDefinition() WorkflowDefinition {
	d := readinessDefinition()
	bound := 200
	d.Languages = []string{"en", "es-MX"}
	d.Materials = []WorkflowMaterial{{ID: "brief", Label: "Discussion brief", Description: "An owner-provided printed brief; no file is fetched."}}
	d.Inputs = []WorkflowField{{ID: "goal", Label: "Session goal", Type: "text", Required: true, MaxLength: &bound}, {ID: "ready", Label: "Ready to discuss", Type: "boolean"}}
	d.Outputs = []WorkflowField{{ID: "count", Label: "Reviewed actions", Type: "number"}}
	return d
}
func TestWorkflowMetadataBoundsTypesAndRoundTrip(t *testing.T) {
	d := metadataDefinition()
	require.Empty(t, ValidateWorkflow(d))
	raw, err := json.Marshal(d)
	require.NoError(t, err)
	var restored WorkflowDefinition
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, d, restored)
	for name, change := range map[string]func(*WorkflowDefinition){
		"duplicate-language":  func(d *WorkflowDefinition) { d.Languages = []string{"en-US", "EN-us"} },
		"unbounded-language":  func(d *WorkflowDefinition) { d.Languages = []string{strings.Repeat("x", 80)} },
		"too-many-languages":  func(d *WorkflowDefinition) { d.Languages = make([]string, 9) },
		"duplicate-material":  func(d *WorkflowDefinition) { d.Materials = append(d.Materials, d.Materials[0]) },
		"blank-label":         func(d *WorkflowDefinition) { d.Materials[0].Label = "  " },
		"byte-bound":          func(d *WorkflowDefinition) { d.Materials[0].Label = strings.Repeat("é", 61) },
		"control-description": func(d *WorkflowDefinition) { d.Materials[0].Description = "hidden\x00instruction" },
		"excess-materials":    func(d *WorkflowDefinition) { d.Materials = make([]WorkflowMaterial, 17) },
		"unsupported-type":    func(d *WorkflowDefinition) { d.Inputs[0].Type = "script" },
		"nontext-length":      func(d *WorkflowDefinition) { d.Inputs[0].Type = "number" },
		"zero-text-length":    func(d *WorkflowDefinition) { v := 0; d.Inputs[0].MaxLength = &v },
		"excess-text-length":  func(d *WorkflowDefinition) { v := 8001; d.Inputs[0].MaxLength = &v },
		"duplicate-output":    func(d *WorkflowDefinition) { d.Outputs = append(d.Outputs, d.Outputs[0]) },
		"excess-inputs":       func(d *WorkflowDefinition) { d.Inputs = make([]WorkflowField, 17) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := metadataDefinition()
			change(&candidate)
			require.NotEmpty(t, ValidateWorkflow(candidate))
		})
	}
}
func TestWorkflowMetadataLegacySerializationStaysIdentical(t *testing.T) {
	d := readinessDefinition()
	before, err := json.Marshal(d)
	require.NoError(t, err)
	d.Languages = []string{}
	d.Materials = []WorkflowMaterial{}
	d.Inputs = []WorkflowField{}
	d.Outputs = []WorkflowField{}
	after, err := json.Marshal(d)
	require.NoError(t, err)
	require.Equal(t, before, after)
	for _, field := range []string{"languages", "materials", "inputs", "outputs"} {
		require.NotContains(t, string(after), `"`+field+`"`)
	}
}
