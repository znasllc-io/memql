package agent

// instructions.go is the Go port of the persona-instruction half of
// the Python voice-agent's realtime_instructions.py: it composes the static
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

import (
	"strings"

	"github.com/znasllc-io/memql/integrations/agentdef"
)

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

	// The identity + personality + domains region comes from the SHARED
	// renderer (agentdef.RenderIdentityBlock, #476/#480) -- the exact same block
	// the cognition text path embeds (prompt_data.go's assistant.identityBlock)
	// -- so voice and text cannot say different things about the agent. The
	// voice-specific framing (the lead line above and the spoken constraints
	// below) is layered AROUND it, never inside it. Empty for a persona with no
	// description / style (the starved-ack case today, until the proto stamps
	// the persona fields -- see persona.go); the neutral lead + constraints
	// still render so the voice stays on-task.
	if block := agentdef.RenderIdentityBlock(personaContract(p)); block != "" {
		lines = append(lines, block)
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

// personaContract projects a voice Persona into the converged generation
// contract (#476) so the voice renderer consumes the SAME agentdef projection
// the text path consumes -- neither modality can read a persona field the other
// can't. The voice Persona's Style maps to the contract's Personality (the
// system-prompt prose); Description, Role, and the voice/avatar fields carry
// across directly. Voice carries no grounding domains on the ack today, so the
// contract's Domains line is simply absent until the proto stamps them.
func personaContract(p Persona) agentdef.AgentGenerationContract {
	return agentdef.BuildGenerationContract(agentdef.ContractInput{
		Name:            p.DisplayName,
		Role:            p.Role,
		Description:     p.Description,
		Personality:     p.Style,
		CanonicalVoice:  p.CanonicalVoice,
		AvatarPersonaID: p.AvatarPersonaID,
		AvatarVendor:    p.AvatarVendor,
	})
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

// RealtimeInstructionsForReply renders the per-response instructions string for
// one conductor-gated realtime response (#432 section 2.4 / 3.3). It is the
// realtime analog of the cascade's per-turn prompt: the conductor's decision
// reaches the executor as the GA's reply text (via VoiceAgentTurnComplete),
// and the speech-to-speech model needs that decision rendered as per-response
// `instructions` that override the session default for this one response.
//
// The conductor already made the turn-shaping decision server-side (mode /
// brevity / suppression collapse into "speak this text" vs "stay silent" over
// the VoiceAgent* seam the cascade shares -- see realtime_executor.go), so the
// rendered directive instructs the model to convey that decided content in the
// persona's voice rather than re-deriving an answer. Empty reply -> empty
// instructions (the caller never calls CreateResponse for a suppressed turn).
func RealtimeInstructionsForReply(reply string) string {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return ""
	}
	lines := []string{
		"Respond now in your persona's voice. Convey the following, naturally " +
			"and concisely, as spoken dialogue -- do not read it verbatim or add " +
			"help-desk scaffolding:",
		reply,
	}
	return strings.Join(lines, "\n")
}
