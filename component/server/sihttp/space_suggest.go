package sihttp

import "encoding/json"

// SpaceSuggestSchemaJSON is the JSON Schema enforced on the space
// suggestion output. Mirrors the fields BuildSpaceSuggestMessages asks
// the model for. agentIds is an array of strings (not enum-constrained)
// because space suggest accepts a dynamic agent roster per call.
var SpaceSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "agentIds", "architecture"],
  "properties": {
    "title": {"type": "string", "description": "3-7 word space title, no quotes."},
    "agentIds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "IDs of selected agents. 1 for standard architecture, 1-3 for polyphon."
    },
    "architecture": {"type": "string", "enum": ["standard", "polyphon"]}
  }
}`)

// SpaceAgent describes an agent available for space-creation suggestions.
type SpaceAgent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
