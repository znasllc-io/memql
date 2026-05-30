package agentdef

import "strings"

// RenderIdentityBlock renders the shared identity region every modality embeds:
// the role/description line, the domains line, and the personality prose. It is
// the modality-neutral core referenced in docs/voice/476-converged-generation-
// contract.md section 3.3 -- the one place persona/identity text is produced, so
// the text prompt (cognitionReply.tmpl's "## YOUR IDENTITY" / "### Personality &
// Instructions" blocks) and the voice session instructions
// (BuildPersonaInstructions) cannot diverge in what they say about the agent.
//
// Deliberately NOT included here, because they are modality framing layered
// AROUND this block by each renderer, not shared content:
//   - the opening "You are <name> ..." sentence (text: "an AI participant in a
//     space"; voice: "the <role> in a live voice conversation");
//   - markdown headers (text-only scaffolding);
//   - the per-turn directive and the spoken/tool-choreography constraints.
//
// Output shape (lines omitted when their field is empty):
//
//	Role: <description>
//	Domains: <d1>, <d2>, ...
//
//	<personality>
//
// Pure and deterministic -> golden-file testable. The text path adopts this in
// the same change that lands the contract; the voice path adopts it in #478, at
// which point a cross-modality golden test pins both call sites to this output.
func RenderIdentityBlock(c AgentGenerationContract) string {
	var head []string
	if c.Description != "" {
		head = append(head, "Role: "+c.Description)
	}
	if len(c.Domains) > 0 {
		head = append(head, "Domains: "+strings.Join(c.Domains, ", "))
	}

	var b strings.Builder
	b.WriteString(strings.Join(head, "\n"))

	if c.Personality != "" {
		if b.Len() > 0 {
			// One blank line between the head lines and the personality prose.
			b.WriteString("\n\n")
		}
		b.WriteString(c.Personality)
	}

	return b.String()
}
