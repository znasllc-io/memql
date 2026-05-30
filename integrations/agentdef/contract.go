// Package agentdef is the single, modality-independent projection of an
// agent's generation definition. It exists so the text path (cognition's
// agentReply loop) and the voice path (the gpt-realtime session bring-up)
// read the SAME fields off the one source of truth, the v1:agents:agent row,
// and therefore cannot drift in persona / knowledge / policy.
//
// This is the build of the converged-generation-contract spike (#476); see
// docs/voice/476-converged-generation-contract.md. The keystone idea: there is
// already exactly one place an agent is defined (v1:agents:agent), and the
// drift the epic (#475) worries about comes from the two modalities reading
// DIFFERENT subsets of that one row through different builders. The fix is one
// projector both call.
//
// The package depends on nothing but the standard library on purpose. Each
// caller adapts its own source -- the cognition text path from the
// ActingAgentIdentity proto, the voice path from the session ack (#478) -- into
// the neutral ContractInput, so agentdef never imports the proto or voice
// packages and there is no import cycle.
package agentdef

import "strings"

// AgentGenerationContract is the single, modality-independent definition of how
// one agent authors a turn. Projected from a v1:agents:agent row (via the
// caller's ContractInput) by BuildGenerationContract; instantiated by the text
// endpoint and the voice endpoint identically in its shared region. Pure and
// deterministic, so it is golden-file testable.
//
// Field groups:
//   - Identity / persona: AgentID..SystemPrompt -- read by both modalities.
//   - Model selection: Provider..MaxTokens -- the resolved SI Router pick; for
//     voice this resolves to gpt-realtime. Recorded so the choice is explicit
//     and auditable rather than implicit-by-runtime.
//   - Grounding selectors: Domains / ToolSlugs / Keywords -- the union the
//     replier computes from capabilities.skillIds. Voice exposes only the
//     low-risk tool subset; privileged tools stay cognition-gated (#475).
//   - Voice projection: CanonicalVoice..VoiceToVoice -- ignored on the text
//     endpoint, populated by the voice bring-up (#478).
type AgentGenerationContract struct {
	AgentID      string
	Name         string
	Role         string // "assistant" | "specialist"
	RoleSlug     string
	Description  string
	Personality  string // the system-prompt prose
	SystemPrompt string // extra instructions

	Provider    string
	Model       string
	PolicyName  string
	Temperature float64
	MaxTokens   int

	Domains   []string
	ToolSlugs []string
	Keywords  []string

	CanonicalVoice  string
	AvatarPersonaID string
	AvatarVendor    string
	VoiceToVoice    bool
}

// ContractInput is the neutral, source-agnostic input to
// BuildGenerationContract. Each caller fills the fields it has from its own
// wire shape (the text path from ActingAgentIdentity; the voice path from the
// session ack); the projector normalizes them identically so both modalities
// get the same definition. Unset fields are simply zero -- a modality that does
// not carry a field (e.g. the text path carries no voice id) leaves it empty.
type ContractInput struct {
	AgentID      string
	Name         string
	Role         string
	RoleSlug     string
	Description  string
	Personality  string
	SystemPrompt string

	Provider    string
	Model       string
	PolicyName  string
	Temperature float64
	MaxTokens   int

	Domains   []string
	ToolSlugs []string
	Keywords  []string

	CanonicalVoice  string
	AvatarPersonaID string
	AvatarVendor    string
	VoiceToVoice    bool
}

// BuildGenerationContract projects a ContractInput into the converged contract.
// Pure; no I/O. Every string is trimmed and every slice is cleaned (each entry
// trimmed, empties dropped) so both modalities receive an identically-normalized
// definition -- neither can render a stray-whitespace variant the other won't.
func BuildGenerationContract(in ContractInput) AgentGenerationContract {
	return AgentGenerationContract{
		AgentID:      strings.TrimSpace(in.AgentID),
		Name:         strings.TrimSpace(in.Name),
		Role:         strings.TrimSpace(in.Role),
		RoleSlug:     strings.TrimSpace(in.RoleSlug),
		Description:  strings.TrimSpace(in.Description),
		Personality:  strings.TrimSpace(in.Personality),
		SystemPrompt: strings.TrimSpace(in.SystemPrompt),

		Provider:    strings.TrimSpace(in.Provider),
		Model:       strings.TrimSpace(in.Model),
		PolicyName:  strings.TrimSpace(in.PolicyName),
		Temperature: in.Temperature,
		MaxTokens:   in.MaxTokens,

		Domains:   cleanSlice(in.Domains),
		ToolSlugs: cleanSlice(in.ToolSlugs),
		Keywords:  cleanSlice(in.Keywords),

		CanonicalVoice:  strings.TrimSpace(in.CanonicalVoice),
		AvatarPersonaID: strings.TrimSpace(in.AvatarPersonaID),
		AvatarVendor:    strings.TrimSpace(in.AvatarVendor),
		VoiceToVoice:    in.VoiceToVoice,
	}
}

// cleanSlice trims each entry and drops empties, preserving order. Returns nil
// for an all-empty or nil input so callers can branch on len() == 0 uniformly.
func cleanSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
