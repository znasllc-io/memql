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

	// Space context (#1470 Option C): WHERE the agent is + WHO she's with. The
	// native realtime path bypasses cognition (the chat path's source of this),
	// so without it the agent has no idea which space she's in. Each line is
	// omitted when its field is empty (a starved ack), keeping the block lean.
	if ctxBlock := buildSpaceContextBlock(p); ctxBlock != "" {
		lines = append(lines, "", ctxBlock)
	}

	// Capability awareness (#1470 Option C): a short, natural summary of what
	// the agent can actually do, so she can ACT on it and answer "what can you
	// do?". Intentionally a fixed, terse summary of the assistant's real tool
	// surface (todos/notes/calendar; driving the app via Takeover; producing
	// files/deliverables by delegating to the planner) rather than a dump of
	// resolved tool names -- a bloated session prompt raises gpt-realtime TTFB
	// (#1439), so this stays surgical.
	lines = append(lines,
		"",
		"What you can do:",
		"- Manage the user's todos, notes, and calendar directly.",
		"- Drive the app on the user's behalf when asked (Takeover): point things out, click, and navigate.",
		"- Produce files and other deliverables by handing the work to your planner, which runs it in the background (see the delegation guidance below).",
	)

	lines = append(lines,
		"",
		"Constraints:",
		"- Stay in character as the persona above across the whole conversation.",
		// #1470: the old line forbade the agent from even knowing her
		// capabilities; the real intent was just "no help-desk scaffolding". She
		// now KNOWS what she can do (above) and may say so naturally when asked --
		// without reciting a menu or narrating tool plumbing.
		"- Speak naturally and concisely. When asked what you can do, answer in a sentence or two from the list above -- do not recite a menu or add help-desk scaffolding.",
		"- Ground answers in the conversation and any provided context. When context does not cover a question, say so rather than inventing facts.",
		// #1470 async-delegation (OWNER REQUIREMENT): heavy/multi-step
		// deliverables go to the planner, and the user is TOLD a task is being
		// created. Terse on purpose -- realtime instructions are token-sensitive
		// (#1439).
		"- For heavy or multi-step deliverables (produce a file or document, write something long, research a topic -- anything best decomposed into tasks), do NOT try to author it inline in voice. Acknowledge naturally and TELL the user a task is being created (e.g. \"sure -- I'll set up a task for that and it'll work on it in the background; meanwhile...\"), then call produceArtifact with a self-contained description of the deliverable. Keep talking; when the result lands, weave it in.",
		// #1430 acknowledge-first tool behaviour: the async function-calling
		// protocol supports background execution, but only the prompt makes
		// the model SAY something before the result lands.
		"- When you call a tool, first say one brief natural acknowledgment (like \"on it -- give me a moment\") and keep the conversation going; the result arrives later as a function output -- weave it in naturally then. Never go silent waiting on a tool, and never promise exact durations.",
		"- A separate per-turn directive may set the mode, brevity, and angle for a given response. Follow it; it overrides these defaults where they conflict.",
	)

	return strings.Join(lines, "\n")
}

// buildSpaceContextBlock renders the labeled space-context block from the
// resolved Persona (#1470 Option C): the space name + purpose and the human
// participants present. Returns "" when no context is populated (a starved
// ack) so the caller skips the block entirely -- keeping the realtime prompt
// lean when there's nothing to say.
func buildSpaceContextBlock(p Persona) string {
	name := strings.TrimSpace(p.SpaceName)
	purpose := strings.TrimSpace(p.SpacePurpose)
	participants := dedupeNonEmpty(p.ParticipantNames)
	if name == "" && purpose == "" && len(participants) == 0 {
		return ""
	}

	lines := []string{"Where you are:"}
	switch {
	case name != "" && purpose != "":
		lines = append(lines, "- You are in the \""+name+"\" space. Its goal: "+purpose+".")
	case name != "":
		lines = append(lines, "- You are in the \""+name+"\" space.")
	case purpose != "":
		lines = append(lines, "- This space's goal: "+purpose+".")
	}
	switch len(participants) {
	case 0:
		// no participant line
	case 1:
		lines = append(lines, "- Person here with you: "+participants[0]+".")
	default:
		lines = append(lines, "- People here with you: "+strings.Join(participants, ", ")+".")
	}
	return strings.Join(lines, "\n")
}

// dedupeNonEmpty trims, drops blanks, and de-duplicates a string slice,
// preserving first-seen order. Used to normalise the participant-name list
// before rendering.
func dedupeNonEmpty(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
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

// RealtimeInstructionsForDirective renders the per-response instructions for a
// conductor GATE decision (#477/#479): a content-free mode+brevity directive the
// model conditions its OWN generation on, NOT authored reply text. This is the
// "WHEN not WHAT" half of epic #475 -- the conductor decides whether/how-briefly
// to speak, the model decides the words. It is the sibling to
// RealtimeInstructionsForReply: that one carries cognition's authored words ("convey
// the following"); this one carries only the turn-shaping directive so the model
// authors natively, so a spoken reply and a typed reply cannot drift in content.
//
// mode / brevity are the wire-string forms of the conductor's DirectiveMode /
// Brevity (taken as plain strings so the voice agent does not import the
// cognition package). An empty/"defer" mode returns "" -- the gate suppresses
// the turn and never calls CreateResponse. Unknown values fall back to the
// substantive primary/normal framing rather than going silent.
func RealtimeInstructionsForDirective(mode, brevity string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	brevity = strings.ToLower(strings.TrimSpace(brevity))

	// Defer / empty -> suppress: the caller must not drive CreateResponse.
	if mode == "" || mode == "defer" {
		return ""
	}

	lines := []string{"Respond now in your own persona's voice. Generate your own reply -- do not wait for or repeat any provided text."}

	switch mode {
	case "brief_ack":
		lines = append(lines, "This is a brief acknowledgment: one short sentence, no agenda.")
	case "chimein":
		lines = append(lines, "Others are also responding this turn. Add only your distinct angle; do not restate what was already said.")
	case toolResultDirectiveMode:
		// #1430: the executor's tool worker injected one or more
		// function_call_output items and is surfacing them at a quiet boundary.
		lines = append(lines, "Results from your earlier tool calls just arrived in the conversation. Tell the user the outcome naturally and concisely; if a result reports an error or timeout, say so plainly and offer a way forward. Skip anything you already told them.")
	default: // "primary" and any unknown mode
		lines = append(lines, "Answer the user directly and substantively.")
	}

	switch brevity {
	case "short":
		lines = append(lines, "Keep it to one short sentence.")
	case "detailed":
		lines = append(lines, "A longer answer is warranted; stay focused.")
	default: // "normal" and any unknown brevity
		lines = append(lines, "Keep it to a few sentences.")
	}

	return strings.Join(lines, "\n")
}
