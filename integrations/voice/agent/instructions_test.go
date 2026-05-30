package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/znasllc-io/memql/integrations/agentdef"
)

func TestBuildPersonaInstructions_NeutralDefault(t *testing.T) {
	// No persona-prompt fields -> neutral default identity, always non-empty.
	out := BuildPersonaInstructions(Persona{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "You are Assistant, the General Assistant in a live voice conversation.")
	assert.Contains(t, out, "Constraints:")
	assert.Contains(t, out, "Stay in character")
	// No style block when style is unset.
	assert.NotContains(t, out, "Style and personality:")
}

func TestBuildPersonaInstructions_FullPersona(t *testing.T) {
	p := Persona{
		DisplayName: "Sofia",
		Role:        "Sales Advisor",
		Description: "Helps customers choose the right plan.",
		Style:       "Warm, concise, never pushy.",
	}
	out := BuildPersonaInstructions(p)

	assert.Contains(t, out, "You are Sofia, the Sales Advisor in a live voice conversation.")
	// Identity + personality now render through the SHARED block (#476/#480):
	// "Role: <description>" then the personality prose.
	assert.Contains(t, out, "Role: Helps customers choose the right plan.")
	assert.Contains(t, out, "Warm, concise, never pushy.")
	assert.Contains(t, out, "Constraints:")

	// The shared description line appears before the personality prose, which
	// appears before the voice constraints.
	assert.Less(t,
		strings.Index(out, "Role: Helps customers"),
		strings.Index(out, "Warm, concise, never pushy."))
	assert.Less(t,
		strings.Index(out, "Warm, concise, never pushy."),
		strings.Index(out, "Constraints:"))
}

// TestBuildPersonaInstructions_EmbedsSharedBlock asserts the voice instructions
// embed the agentdef shared identity block verbatim -- the cross-modality
// convergence guarantee (#476 section 7.2): voice and text describe the agent
// through the SAME renderer, so they cannot diverge. The text path embeds the
// same block via prompt_data.go's assistant.identityBlock.
func TestBuildPersonaInstructions_EmbedsSharedBlock(t *testing.T) {
	p := Persona{
		DisplayName: "Sofia",
		Role:        "Sales Advisor",
		Description: "Helps customers choose the right plan.",
		Style:       "Warm, concise, never pushy.",
	}
	shared := agentdef.RenderIdentityBlock(personaContract(p))
	assert.NotEmpty(t, shared)
	assert.Contains(t, BuildPersonaInstructions(p), shared,
		"voice instructions must embed the shared identity block verbatim")
}

func TestBuildPersonaInstructions_TrimsWhitespaceFields(t *testing.T) {
	// Blank-only persona fields are treated as unset -> neutral default.
	p := Persona{DisplayName: "   ", Role: "\t", Description: "  ", Style: " "}
	out := BuildPersonaInstructions(p)
	assert.Contains(t, out, "You are Assistant, the General Assistant")
	assert.NotContains(t, out, "Style and personality:")
}

func TestBuildSessionPersona(t *testing.T) {
	t.Setenv("POLYPHON_VOICE_PROVIDER", "openai")
	p := ResolvePersona(SessionAck{CanonicalVoice: "alto"}, Config{})

	sp := BuildSessionPersona(p)
	assert.NotEmpty(t, sp.Instructions)
	assert.Equal(t, ResolveRealtimeVoice(p), sp.Voice)
	assert.Equal(t, BuildPersonaInstructions(p), sp.Instructions)
}

func TestRealtimeInstructionsForReply(t *testing.T) {
	// A decided reply is rendered as a per-response directive that carries the
	// content verbatim with spoken-register framing (#432 conductor gate).
	out := RealtimeInstructionsForReply("The cloud spend is the place to start.")
	assert.Contains(t, out, "The cloud spend is the place to start.")
	assert.Contains(t, out, "persona's voice")

	// An empty / blank reply renders empty instructions: the caller never
	// drives response.create for a suppressed turn.
	assert.Empty(t, RealtimeInstructionsForReply(""))
	assert.Empty(t, RealtimeInstructionsForReply("   "))
}
