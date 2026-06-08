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

// TestBuildPromptData_ProductionDirectiveTrusted is the agent-side half of the
// memql#1102 regression: the planner forwards the trusted produce-flow
// scaffolding via hints["production_directive"] and the genuine user goal as a
// user-role history message. buildPromptData must surface the directive as the
// top-level `productionDirective` field (rendered by agentReply.tmpl as a
// TRUSTED, un-bracketed block) and keep the history message untouched (it's
// rendered inside the untrusted history block). The two must never be conflated.
func TestBuildPromptData_ProductionDirectiveTrusted(t *testing.T) {
	const directive = "PRODUCE THIS DELIVERABLE NOW. ... do NOT call produceArtifact ..."
	const goal = "create a list of the top 10 most beautiful birds"
	msg := &memqlv1.AgentGenerateTurnMsg{
		AgentId: "v1:agents:agent/ga",
		History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: goal}},
		Hints: map[string]string{
			"trigger":              "plan_approved",
			"plan_id":              "v1:planner:plan:p1",
			"production_directive": directive,
		},
	}

	data := buildPromptData(msg)

	// Directive surfaced as a trusted top-level field.
	assert.Equal(t, directive, data["productionDirective"],
		"production_directive hint must surface as the trusted productionDirective field")
	assert.Equal(t, true, data["planApprovedTrigger"])

	// The untrusted history message stays the raw user goal -- the directive
	// must NOT have been folded into it.
	history, ok := data["history"].([]map[string]any)
	assert.True(t, ok, "history must be present")
	assert.Len(t, history, 1)
	assert.Equal(t, goal, history[0]["content"])
	assert.NotContains(t, history[0]["content"], "PRODUCE THIS DELIVERABLE NOW")
}

// TestBuildPromptData_NoProductionDirectiveByDefault asserts a normal turn
// (no hint) carries no productionDirective field, so the trusted block stays
// off for non-produce turns.
func TestBuildPromptData_NoProductionDirectiveByDefault(t *testing.T) {
	data := buildPromptData(&memqlv1.AgentGenerateTurnMsg{AgentId: "v1:agents:agent/ga"})
	_, has := data["productionDirective"]
	assert.False(t, has, "no production_directive hint -> no productionDirective field")
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
