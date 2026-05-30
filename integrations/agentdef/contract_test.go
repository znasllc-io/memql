package agentdef

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildGenerationContract_TrimsAndCleans asserts the projector normalizes
// every field identically: strings trimmed, slices trimmed + empties dropped,
// scalars passed through. This is the guarantee that two modalities projecting
// the same row get a byte-identical definition.
func TestBuildGenerationContract_TrimsAndCleans(t *testing.T) {
	got := BuildGenerationContract(ContractInput{
		AgentID:        "  v1:agents:agent/ga  ",
		Name:           "  Sofia  ",
		Role:           " assistant ",
		RoleSlug:       " general ",
		Description:    "  Sales Specialist  ",
		Personality:    "  Warm and concise.  ",
		SystemPrompt:   "  Always cite sources.  ",
		Provider:       " openai ",
		Model:          " gpt-realtime ",
		PolicyName:     " balancedChat ",
		Temperature:    0.7,
		MaxTokens:      1024,
		Domains:        []string{" sales ", "", "   ", "support"},
		ToolSlugs:      []string{"queryGraph", "  "},
		Keywords:       []string{" pricing "},
		CanonicalVoice: " alto ",
		VoiceToVoice:   true,
	})

	assert.Equal(t, "v1:agents:agent/ga", got.AgentID)
	assert.Equal(t, "Sofia", got.Name)
	assert.Equal(t, "assistant", got.Role)
	assert.Equal(t, "general", got.RoleSlug)
	assert.Equal(t, "Sales Specialist", got.Description)
	assert.Equal(t, "Warm and concise.", got.Personality)
	assert.Equal(t, "Always cite sources.", got.SystemPrompt)
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "gpt-realtime", got.Model)
	assert.Equal(t, "balancedChat", got.PolicyName)
	assert.Equal(t, 0.7, got.Temperature)
	assert.Equal(t, 1024, got.MaxTokens)
	// Empty / whitespace-only slice entries dropped, order preserved.
	assert.Equal(t, []string{"sales", "support"}, got.Domains)
	assert.Equal(t, []string{"queryGraph"}, got.ToolSlugs)
	assert.Equal(t, []string{"pricing"}, got.Keywords)
	assert.Equal(t, "alto", got.CanonicalVoice)
	assert.True(t, got.VoiceToVoice)
}

// TestBuildGenerationContract_EmptySlicesNil asserts all-empty / nil slices
// project to nil so callers branch on len() == 0 uniformly across modalities.
func TestBuildGenerationContract_EmptySlicesNil(t *testing.T) {
	got := BuildGenerationContract(ContractInput{
		Name:    "GA",
		Domains: []string{"", "  "},
	})
	assert.Nil(t, got.Domains)
	assert.Nil(t, got.ToolSlugs)
	assert.Nil(t, got.Keywords)
}

// TestBuildGenerationContract_Deterministic asserts the projector is pure --
// the same input yields an equal contract every call (the property the
// golden/consistency tests rely on).
func TestBuildGenerationContract_Deterministic(t *testing.T) {
	in := ContractInput{Name: "Sofia", Description: "Sales", Domains: []string{"sales"}}
	assert.Equal(t, BuildGenerationContract(in), BuildGenerationContract(in))
}
