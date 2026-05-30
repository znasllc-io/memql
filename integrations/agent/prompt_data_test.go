package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestBuildPromptData_AssistantFromContract asserts the text path's assistant
// block is projected through the converged generation contract (#476/#480):
// the existing keys are preserved (byte-identical to the prior inline mapping,
// trim + omit-empty) and the shared identityBlock is added for the voice path
// to adopt in #478.
func TestBuildPromptData_AssistantFromContract(t *testing.T) {
	msg := &memqlv1.AgentGenerateTurnMsg{
		AgentId: "v1:agents:agent/ga",
		ActingAgent: &memqlv1.ActingAgentIdentity{
			Id:           "v1:agents:agent/ga",
			Name:         "  Sofia  ", // trimmed by the contract
			Description:  "Sales Specialist",
			Personality:  "Warm and concise.",
			SystemPrompt: "Cite sources.",
			Role:         "assistant",
			Domains:      []string{"sales", "support"},
			Keywords:     []string{"pricing"},
			Tools:        []string{"queryGraph"},
		},
	}

	data := buildPromptData(msg)
	assistant, ok := data["assistant"].(map[string]any)
	assert.True(t, ok, "assistant block must be present")

	assert.Equal(t, "Sofia", assistant["name"])
	assert.Equal(t, "v1:agents:agent/ga", assistant["id"])
	assert.Equal(t, "Sales Specialist", assistant["description"])
	assert.Equal(t, "Warm and concise.", assistant["personality"])
	assert.Equal(t, "Cite sources.", assistant["systemPrompt"])
	assert.Equal(t, "assistant", assistant["role"])
	assert.Equal(t, []string{"sales", "support"}, assistant["domains"])
	assert.Equal(t, []string{"pricing"}, assistant["keywords"])
	assert.Equal(t, []string{"queryGraph"}, assistant["tools"])

	// Shared identity region (RenderIdentityBlock), additive for #478.
	assert.Equal(t,
		"Role: Sales Specialist\nDomains: sales, support\n\nWarm and concise.",
		assistant["identityBlock"])
}

// TestBuildPromptData_NoActingAgentFallsBack asserts the minimal-routing path
// (no ActingAgent) still falls back to the agent id as name and renders no
// identity block -- unchanged from before the contract projection.
func TestBuildPromptData_NoActingAgentFallsBack(t *testing.T) {
	data := buildPromptData(&memqlv1.AgentGenerateTurnMsg{AgentId: "v1:agents:agent/ga"})
	assistant, ok := data["assistant"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "v1:agents:agent/ga", assistant["name"])
	_, hasBlock := assistant["identityBlock"]
	assert.False(t, hasBlock, "no acting agent -> no identity block")
}
