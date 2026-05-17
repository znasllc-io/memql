package sihttp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// AgentCardSummarySchemaJSON is the JSON Schema for the agent.created
// canvas card's dynamic body. The card itself doesn't take any
// actions -- it's a presentation-only welcome moment for a brand-new
// agent in the user's roster -- so the LLM's job is purely
// descriptive: a short tagline, a paragraph that explains who the
// agent is and what they're for, and 2-4 concrete use cases the user
// can imagine inviting them to help with.
//
// Distinct from the existing `agents` domain (which takes a free-text
// description and proposes a NAME / personality / capabilities for a
// new agent). This domain is the inverse: an agent already exists
// (with a stable name + role + tools), and we just want a friendly
// blurb for the canvas.
var AgentCardSummarySchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["tagline", "blurb", "useCases"],
  "properties": {
    "tagline": {
      "type": "string",
      "description": "A 4-8 word headline introducing the agent. No agent name, no quotes, no trailing punctuation."
    },
    "blurb": {
      "type": "string",
      "description": "1 to 2 sentence explanation of what this agent is for and how the user can collaborate with them. <= 280 chars total. Plain prose, no markdown, no quotes."
    },
    "useCases": {
      "type": "array",
      "minItems": 2,
      "maxItems": 4,
      "items": {
        "type": "string",
        "description": "A concrete user-voice example of when to ask this agent for help. <= 100 chars. Action-first phrasing (e.g. 'Draft my Q3 board update', 'Reconcile last month's expenses'). No agent name, no quotes."
      }
    }
  }
}`)

// AgentCardSummaryInput carries everything the prompt needs to
// describe an agent without round-tripping the whole agent record.
// The frontend hands these in as the payload to
// aiSuggest({domain: "agentCardSummary", payload: ...}).
type AgentCardSummaryInput struct {
	Name        string
	Role        string // "general_assistant" or "specialist"
	RoleSlug    string // "it_support" / "accounting_finance" / etc.
	Description string
	Personality string
	Domains     []string
	Keywords    []string
	Tools       []string
}

// BuildAgentCardSummarySuggestMessages assembles the system + user
// prompts for the canvas-card summary. The system prompt biases the
// model toward concrete, action-first use cases (the part the user
// said is most often stale -- generic "Your team has a new
// collaborator on board" was the placeholder); the user prompt
// dumps the agent's actual configuration so the model has something
// real to anchor on.
func BuildAgentCardSummarySuggestMessages(in AgentCardSummaryInput) []common.ChatMessage {
	systemPrompt := `You write a concise welcome-card summary for a newly-created AI agent on a CoPresent canvas.

You receive the agent's actual configuration (name, role, tools, knowledge domains). Your job: describe THIS agent specifically, not a generic AI helper.

Rules:
- "tagline": a punchy 4-8 word headline. Don't include the agent's name (the card displays it separately). Examples: "Your accounting and finance partner.", "Numbers, ledgers, and quarterly reports."
- "blurb": 1-2 sentences (<= 280 chars). Explain what this agent is for AND how the user can collaborate with them. Mention their actual specialty (driven by role + domains + tools), not generic "I'm here to help" filler.
- "useCases": 2-4 concrete, action-first examples a user could literally type into chat. Each <= 100 chars. Phrase as imperatives or short requests (e.g. "Reconcile last month's expenses", "Draft a contract amendment", "Summarize this PDF"). Don't repeat the agent's name in each one. Don't include trailing punctuation.
- Tone: warm, professional, specific. Avoid "I am an AI" / "as a large language model" / "delighted to assist". Avoid generic SaaS marketing.
- All strings: no quotes, no markdown formatting, no emoji.
- Return ONLY valid JSON matching the schema.`

	// Build a compact user prompt. The model sees the agent's actual
	// configuration verbatim so it can't drift into generic territory.
	var lines []string
	lines = append(lines, fmt.Sprintf("Name: %s", strings.TrimSpace(in.Name)))
	if role := strings.TrimSpace(in.Role); role != "" {
		lines = append(lines, fmt.Sprintf("Role: %s", role))
	}
	if slug := strings.TrimSpace(in.RoleSlug); slug != "" {
		lines = append(lines, fmt.Sprintf("Specialty: %s", slug))
	}
	if desc := strings.TrimSpace(in.Description); desc != "" {
		lines = append(lines, fmt.Sprintf("Description: %s", desc))
	}
	if pers := strings.TrimSpace(in.Personality); pers != "" {
		// Personality is sometimes long-form prompt text. Cap it so we
		// don't blow the prompt budget on one field.
		if len(pers) > 600 {
			pers = pers[:600] + "…"
		}
		lines = append(lines, fmt.Sprintf("Personality: %s", pers))
	}
	if len(in.Domains) > 0 {
		lines = append(lines, fmt.Sprintf("Knowledge domains: %s", strings.Join(in.Domains, ", ")))
	}
	if len(in.Keywords) > 0 {
		lines = append(lines, fmt.Sprintf("Keywords: %s", strings.Join(in.Keywords, ", ")))
	}
	if len(in.Tools) > 0 {
		lines = append(lines, fmt.Sprintf("Tools: %s", strings.Join(in.Tools, ", ")))
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

// PostProcessAgentCardSummarySuggestion normalises the LLM output --
// strips wrapping quotes / leading & trailing whitespace on each
// string, and clamps lengths defensively so a chatty model can't
// blow up the card layout. The frontend additionally clamps for
// display, so this is the anti-runaway safety net.
func PostProcessAgentCardSummarySuggestion(suggestion map[string]any) {
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

func clampSuggestString(s string, maxLen int) string {
	out := strings.TrimSpace(s)
	out = strings.Trim(out, "\"'“”‘’")
	out = strings.TrimSpace(out)
	if len(out) > maxLen {
		out = strings.TrimSpace(out[:maxLen])
	}
	return out
}
