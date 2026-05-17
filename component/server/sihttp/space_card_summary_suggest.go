package sihttp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// SpaceCardSummarySchemaJSON is the JSON Schema for the space.created
// canvas card's dynamic body. Mirrors the agent.created and
// group.created card surfaces (tagline + blurb + use cases) so users
// get the same kind of welcome-card moment when they create a space
// as when they create an agent or a group.
//
// Distinct from the existing `spaces` domain (which proposes a NEW
// space from a free-text description -- name + description +
// suggested agent assignments) and the `spaceTitle` domain (which
// turns a one-line purpose into a compact title). This domain is
// the inverse of both: a space already exists with stable name +
// description, and we just want a friendly welcome blurb for the
// canvas card.
var SpaceCardSummarySchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["tagline", "blurb", "useCases"],
  "properties": {
    "tagline": {
      "type": "string",
      "description": "A 4-8 word headline describing the space's purpose. No space name, no quotes, no trailing punctuation."
    },
    "blurb": {
      "type": "string",
      "description": "1 to 2 sentence explanation of what this space is for and how the user can collaborate inside it. <= 280 chars total. Plain prose, no markdown, no quotes."
    },
    "useCases": {
      "type": "array",
      "minItems": 2,
      "maxItems": 4,
      "items": {
        "type": "string",
        "description": "A concrete user-voice example of when to engage this space's agents. <= 100 chars. Action-first phrasing (e.g. 'Pull together the launch retrospective notes', 'Draft the Q3 OKR proposal'). No space name, no quotes."
      }
    }
  }
}`)

// SpaceCardSummaryInput carries everything the prompt needs to
// describe a space without round-tripping the whole space record
// plus its participants. The frontend hands these in as the payload
// to aiSuggest({domain: "spaceCardSummary", payload: ...}).
type SpaceCardSummaryInput struct {
	Name        string
	Description string
	Kind        string // "regular" or "daily"
	Private     bool
	// AgentRoles is an optional small set of role labels for the
	// agents present in the space ("IT Support", "Accounting", etc.)
	// so the model can ground use cases in the actual specialties
	// available to invoke. Pass the user's GA + any specialists they
	// added at creation time.
	AgentRoles []string
}

// BuildSpaceCardSummarySuggestMessages assembles the system + user
// prompts for the space canvas-card summary. Same structure as the
// agent / group siblings: action-first use cases anchored on the
// space's actual configuration.
func BuildSpaceCardSummarySuggestMessages(in SpaceCardSummaryInput) []common.ChatMessage {
	systemPrompt := `You write a concise welcome-card summary for a newly-created collaboration space on a CoPresent canvas.

You receive the space's actual configuration (name, description, kind, privacy, and the agent specialties present). Your job: describe THIS space specifically, not a generic team room.

Rules:
- "tagline": a punchy 4-8 word headline describing the space's purpose. Don't include the space's name (the card displays it separately). Examples: "Where the launch team coordinates daily.", "Quarterly board prep, end to end."
- "blurb": 1-2 sentences (<= 280 chars). Explain what this space is for AND how participants collaborate inside it. Mention the space's actual focus (driven by description + agent specialties), not generic "real-time collaboration" filler.
- "useCases": 2-4 concrete, action-first examples a user could literally type into chat to engage the space's agents. Each <= 100 chars. Phrase as imperatives or short requests (e.g. "Pull together the launch retrospective notes", "Draft the Q3 OKR proposal", "Summarize this morning's standup"). Don't repeat the space's name. Don't include trailing punctuation.
- For daily-kind spaces, lean toward personal-journal / today-focused use cases (e.g. "Recap what I worked on this morning", "Plan tomorrow's priorities").
- Tone: warm, professional, specific. Avoid "this is a space for..." filler. Avoid generic SaaS marketing.
- All strings: no quotes, no markdown formatting, no emoji.
- Return ONLY valid JSON matching the schema.`

	var lines []string
	lines = append(lines, fmt.Sprintf("Name: %s", strings.TrimSpace(in.Name)))
	if desc := strings.TrimSpace(in.Description); desc != "" {
		// Description is the highest-signal field; give the model the
		// full thing (capped to keep prompt budget sane).
		if len(desc) > 800 {
			desc = desc[:800] + "…"
		}
		lines = append(lines, fmt.Sprintf("Description: %s", desc))
	}
	if k := strings.TrimSpace(in.Kind); k != "" {
		lines = append(lines, fmt.Sprintf("Kind: %s", k))
	}
	if in.Private {
		lines = append(lines, "Privacy: private (single owner; invitees only)")
	} else {
		lines = append(lines, "Privacy: open (visible to other participants in the workspace)")
	}
	if len(in.AgentRoles) > 0 {
		lines = append(lines, fmt.Sprintf("Agent specialties present: %s", strings.Join(in.AgentRoles, ", ")))
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

// PostProcessSpaceCardSummarySuggestion normalises the LLM output --
// strips wrapping quotes / leading & trailing whitespace on each
// string, and clamps lengths defensively so a chatty model can't
// blow up the card layout. Reuses the same clampSuggestString helper
// the agent + group card-summary post-processors use.
func PostProcessSpaceCardSummarySuggestion(suggestion map[string]any) {
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
