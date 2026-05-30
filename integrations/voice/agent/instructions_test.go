package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	assert.Contains(t, out, "Helps customers choose the right plan.")
	assert.Contains(t, out, "Style and personality:")
	assert.Contains(t, out, "Warm, concise, never pushy.")
	assert.Contains(t, out, "Constraints:")

	// Description appears before the style block.
	assert.Less(t,
		strings.Index(out, "Helps customers"),
		strings.Index(out, "Style and personality:"))
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
