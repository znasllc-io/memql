package agent

// instructions.go is the Go port of the persona-instruction half of
// voice-agent/voice_agent/realtime_instructions.py: it composes the static
// realtime session "instructions" string from a Persona's identity + style,
// trimmed to identity + style + spoken constraints. The conductor layers the
// per-turn directive (mode / brevity / angle) ON TOP of this via the
// per-response instructions (#455 cascade path / #457 realtime path), so the
// static block stays small.
//
// This is the realtime analog of memQL's cognition agent prompt (the
// "YOUR IDENTITY" / "Personality & Instructions" blocks in
// dsl/cognition/prompts/cognitionReply.tmpl). It always returns a non-empty
// string: when the persona carries no role / style the neutral default
// identity is rendered so the realtime voice stays on-task instead of
// free-wheeling. Pure / deterministic; unit-tested without a session.

import "strings"

const (
	// defaultPersonaName / defaultPersonaRole are the neutral identity used
	// when the persona has no stamped name / role (the ack does not carry
	// these fields yet -- see persona.go). Mirrors
	// realtime_instructions.py::DEFAULT_PERSONA_NAME / DEFAULT_PERSONA_ROLE.
	defaultPersonaName = "Assistant"
	defaultPersonaRole = "General Assistant"
)

func personaName(p Persona) string {
	if name := strings.TrimSpace(p.DisplayName); name != "" {
		return name
	}
	return defaultPersonaName
}

func personaRole(p Persona) string {
	if role := strings.TrimSpace(p.Role); role != "" {
		return role
	}
	return defaultPersonaRole
}

// BuildPersonaInstructions composes the static realtime session instructions
// from the persona. Mirrors realtime_instructions.py::build_persona_instructions.
//
// Always returns a non-empty string: an unset persona renders the neutral
// default identity. The voice-specific constraints mirror the cascade prompt's
// spoken-register guidance (concise, no help-desk scaffolding, stay in
// character) but are tuned for speech.
func BuildPersonaInstructions(p Persona) string {
	name := personaName(p)
	role := personaRole(p)

	lines := []string{
		"You are " + name + ", the " + role + " in a live voice conversation.",
	}

	if description := strings.TrimSpace(p.Description); description != "" {
		lines = append(lines, description)
	}

	if style := strings.TrimSpace(p.Style); style != "" {
		lines = append(lines, "", "Style and personality:", style)
	}

	lines = append(lines,
		"",
		"Constraints:",
		"- Stay in character as the persona above across the whole conversation.",
		"- Speak naturally and concisely. Do not recite your capabilities or add help-desk scaffolding.",
		"- Ground answers in the conversation and any provided context. When context does not cover a question, say so rather than inventing facts.",
		"- A separate per-turn directive may set the mode, brevity, and angle for a given response. Follow it; it overrides these defaults where they conflict.",
	)

	return strings.Join(lines, "\n")
}

// SessionPersona is the resolved static configuration for a realtime session:
// the instruction string + the provider voice id, both always populated (the
// persona falls back to a neutral default, the voice to the catalog default).
// The realtime executor (#457) wires these unconditionally. Mirrors
// realtime_instructions.py::RealtimeSessionPersona.
type SessionPersona struct {
	Instructions string
	Voice        string
}

// BuildSessionPersona resolves the static realtime session persona
// (instructions + voice) in one step, so the executor seam (#457) takes
// exactly one hook call. Mirrors realtime_instructions.py::build_session_persona.
//
// The voice is resolved through the canonical catalog (voices.go) via
// ResolveRealtimeVoice -- the catalog stays the single source of truth.
func BuildSessionPersona(p Persona) SessionPersona {
	return SessionPersona{
		Instructions: BuildPersonaInstructions(p),
		Voice:        ResolveRealtimeVoice(p),
	}
}
