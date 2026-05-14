package sihttp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/visionarys-io/memql/core/common"
)

// GroupDescriptionSuggestSchemaJSON is the JSON Schema for the
// lightweight "group description from name" suggestion path. The
// CreateGroup modal mirrors the Configure-manually flow on the
// space side: the user types a Group name, and we derive a one-line
// description from it (e.g. "Sales Onboarding" -> "Group focused on
// onboarding new clients to the sales pipeline."). Strict
// single-field output -- no name suggestion, no member rosters --
// so the fallback is a short spinner + an instantly-editable
// description input.
//
// Why a separate domain rather than reusing the existing `groups`
// domain: the `groups` domain expects a long free-text description
// and returns name + description + suggested member ids. This domain
// is the inverse: it takes a (typically much shorter) name and
// produces ONLY a description. Sharing the schema would mean either
// the model picks unwanted fields or the frontend has to post-strip
// them; cheaper to give the model a focused contract.
var GroupDescriptionSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["description"],
  "properties": {
    "description": {
      "type": "string",
      "description": "One-line description (1-2 sentences) of what this group is for, derived from its name. Plain prose, no quotes, no trailing punctuation other than a single period."
    }
  }
}`)

// BuildGroupDescriptionSuggestMessages builds the system and user
// prompts for description derivation from a group name. The system
// prompt biases the model toward describing the group's PURPOSE /
// TYPICAL ACTIVITY rather than just paraphrasing the name (so
// "Sales" doesn't come back as "A group named sales").
func BuildGroupDescriptionSuggestMessages(name string) []common.ChatMessage {
	systemPrompt := `You turn a short group name into a one-line description of what that group is for.

Rules:
- 1 to 2 sentences, total length <= 240 characters.
- Describe the group's PURPOSE or typical activity, not the literal name.
- No quotes, no markdown, no leading/trailing whitespace.
- End with a single period (no trailing punctuation otherwise).
- Plain prose, third-person framing (e.g. "Group focused on…", "Team responsible for…").
- Return ONLY valid JSON: { "description": "<description>" }`

	userPrompt := fmt.Sprintf(`Group name: %q`, strings.TrimSpace(name))

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// PostProcessGroupDescriptionSuggestion normalises the description --
// trims whitespace, strips wrapping quotes, and clamps the upper
// length so a chatty model can't blow up the modal layout. The
// frontend clamps a second time to GROUP_DESCRIPTION_MAX_LENGTH; this
// is the anti-runaway safety net.
func PostProcessGroupDescriptionSuggestion(suggestion map[string]any) {
	raw, ok := suggestion["description"].(string)
	if !ok {
		return
	}
	desc := strings.TrimSpace(raw)
	desc = strings.Trim(desc, "\"'“”‘’")
	desc = strings.TrimSpace(desc)
	// Belt: cap at 280 chars before frontend trim.
	if len(desc) > 280 {
		desc = strings.TrimSpace(desc[:280])
	}
	suggestion["description"] = desc
}
