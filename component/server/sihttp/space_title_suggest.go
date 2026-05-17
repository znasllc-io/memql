package sihttp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/core/common"
)

// SpaceTitleSuggestSchemaJSON is the JSON Schema for the lightweight
// "space title from purpose" suggestion path. The CreateSpaceModal
// Configure-manually flow now takes a one-liner Purpose from the user
// ("Plan Q3 roadmap", "Talk to ops about the Houston facility") and
// asks the model to derive a compact, action-oriented name. Strict
// single-field output -- no agent selection, no architecture -- so
// the fallback is a short spinner + an instantly-editable title.
var SpaceTitleSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["title"],
  "properties": {
    "title": {
      "type": "string",
      "description": "3-6 word title summarising the purpose. Title case. No trailing punctuation, no quotes."
    }
  }
}`)

// BuildSpaceTitleSuggestMessages builds the system and user prompts
// for title derivation. Kept deliberately short -- the input is a
// single-sentence purpose, and the output is just a title, so there's
// no need for the lengthy rule list the full space-config suggest
// uses. The system prompt biases the model toward action-oriented
// titles ("Houston Facility Discussion") rather than literal echoes
// of the purpose ("Talk to ops about the Houston facility").
func BuildSpaceTitleSuggestMessages(purpose string) []common.ChatMessage {
	systemPrompt := `You turn a short purpose sentence into a compact space title.

Rules:
- 3 to 6 words total.
- Title Case.
- No quotes, no trailing punctuation.
- Action-oriented noun phrase (e.g. "Q3 Roadmap Planning", "Houston Facility Discussion") -- do not just echo the input verbatim.
- Return ONLY valid JSON: { "title": "<title>" }`

	userPrompt := fmt.Sprintf(`Purpose: %q`, strings.TrimSpace(purpose))

	return []common.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}
}

// PostProcessSpaceTitleSuggestion normalises the title -- trims
// whitespace, strips wrapping quotes, and clamps to a reasonable
// upper bound so a chatty model can't blow up the modal layout. We
// do NOT clamp to SPACE_NAME_MAX_LENGTH here (that's frontend
// territory); clamping twice would fight the client's editor.
func PostProcessSpaceTitleSuggestion(suggestion map[string]any) {
	raw, ok := suggestion["title"].(string)
	if !ok {
		return
	}
	title := strings.TrimSpace(raw)
	title = strings.Trim(title, "\"'\u201c\u201d\u2018\u2019")
	title = strings.TrimSpace(title)
	// Belt: cap at 120 chars. Frontend clamps harder to
	// SPACE_NAME_MAX_LENGTH (60); this is the anti-runaway guard.
	if len(title) > 120 {
		title = strings.TrimSpace(title[:120])
	}
	suggestion["title"] = title
}
