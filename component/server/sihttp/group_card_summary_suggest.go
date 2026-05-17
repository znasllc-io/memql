package sihttp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// GroupCardSummarySchemaJSON is the JSON Schema for the group.created
// canvas card's dynamic body. Mirrors the agent.created card surface
// (tagline + blurb + use cases) so users get the same kind of
// welcome-card moment when they create a group as when they create
// an agent.
//
// Distinct from the existing `groups` domain (which proposes a NEW
// group from a free-text description -- name + description +
// suggested member ids). This domain is the inverse: a group already
// exists with stable name + description + members + agents, and we
// just want a friendly welcome blurb for the canvas card.
var GroupCardSummarySchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["tagline", "blurb", "useCases"],
  "properties": {
    "tagline": {
      "type": "string",
      "description": "A 4-8 word headline describing the group's purpose. No group name, no quotes, no trailing punctuation."
    },
    "blurb": {
      "type": "string",
      "description": "1 to 2 sentence explanation of what this group is for and how the user can collaborate inside it. <= 280 chars total. Plain prose, no markdown, no quotes."
    },
    "useCases": {
      "type": "array",
      "minItems": 2,
      "maxItems": 4,
      "items": {
        "type": "string",
        "description": "A concrete user-voice example of when to engage this group's agents. <= 100 chars. Action-first phrasing (e.g. 'Schedule a sales-onboarding kickoff', 'Share the new pricing one-pager'). No group name, no quotes."
      }
    }
  }
}`)

// GroupCardSummaryInput carries everything the prompt needs to
// describe a group without round-tripping the whole group record
// plus its membership. The frontend hands these in as the payload
// to aiSuggest({domain: "groupCardSummary", payload: ...}).
type GroupCardSummaryInput struct {
	Name        string
	Description string
	MemberCount int
	AgentCount  int
	// AgentRoles is an optional small set of role labels for the
	// agents assigned to the group ("IT Support", "Accounting", etc.)
	// so the model can ground use cases in the actual specialties
	// available to invite.
	AgentRoles []string
}

// BuildGroupCardSummarySuggestMessages assembles the system + user
// prompts for the group canvas-card summary. Mirrors the
// agent.created surface: concrete, action-first use cases (the part
// the user said is most often stale on these cards) anchored on
// the group's actual configuration.
func BuildGroupCardSummarySuggestMessages(in GroupCardSummaryInput) []common.ChatMessage {
	systemPrompt := `You write a concise welcome-card summary for a newly-created collaboration group on a CoPresent canvas.

You receive the group's actual configuration (name, description, member/agent counts, agent specialties). Your job: describe THIS group specifically, not a generic team space.

Rules:
- "tagline": a punchy 4-8 word headline describing the group's purpose. Don't include the group's name (the card displays it separately). Examples: "Where new sales hires get up to speed.", "Quarterly close, owned end to end."
- "blurb": 1-2 sentences (<= 280 chars). Explain what this group is for AND how members collaborate inside it. Mention the group's actual focus (driven by description + agent specialties), not generic "team collaboration" filler.
- "useCases": 2-4 concrete, action-first examples a user could literally type into chat to engage the group's agents. Each <= 100 chars. Phrase as imperatives or short requests (e.g. "Schedule a sales-onboarding kickoff", "Share the new pricing one-pager", "Roll up this week's pipeline notes"). Don't repeat the group's name in each one. Don't include trailing punctuation.
- Tone: warm, professional, specific. Avoid "this is a group for..." filler. Avoid generic SaaS marketing.
- All strings: no quotes, no markdown formatting, no emoji.
- Return ONLY valid JSON matching the schema.`

	var lines []string
	lines = append(lines, fmt.Sprintf("Name: %s", strings.TrimSpace(in.Name)))
	if desc := strings.TrimSpace(in.Description); desc != "" {
		// Description tends to be the highest-signal field for groups;
		// give the model the full thing (capped to keep prompt budget
		// sane).
		if len(desc) > 800 {
			desc = desc[:800] + "…"
		}
		lines = append(lines, fmt.Sprintf("Description: %s", desc))
	}
	lines = append(lines, fmt.Sprintf("Members: %d", in.MemberCount))
	lines = append(lines, fmt.Sprintf("Agents: %d", in.AgentCount))
	if len(in.AgentRoles) > 0 {
		lines = append(lines, fmt.Sprintf("Agent specialties: %s", strings.Join(in.AgentRoles, ", ")))
	}

	userPrompt := strings.Join(lines, "\n")
	if userPrompt == "" {
		userPrompt = "Name: (unknown)"
	}

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// PostProcessGroupCardSummarySuggestion normalises the LLM output --
// strips wrapping quotes / leading & trailing whitespace on each
// string, and clamps lengths defensively so a chatty model can't
// blow up the card layout. Reuses the same clampSuggestString helper
// the agent-card-summary post-processor uses.
func PostProcessGroupCardSummarySuggestion(suggestion map[string]any) {
	if tagline, ok := suggestion["tagline"].(string); ok {
		suggestion["tagline"] = clampSuggestString(tagline, 80)
	}
	if blurb, ok := suggestion["blurb"].(string); ok {
		suggestion["blurb"] = clampSuggestString(blurb, 320)
	}
	if rawCases, ok := suggestion["useCases"].([]any); ok {
		cleaned := make([]any, 0, len(rawCases))
		for _, item := range rawCases {
			s, ok := item.(string)
			if !ok {
				continue
			}
			c := clampSuggestString(s, 110)
			if c == "" {
				continue
			}
			cleaned = append(cleaned, c)
			if len(cleaned) >= 4 {
				break
			}
		}
		suggestion["useCases"] = cleaned
	}
}
