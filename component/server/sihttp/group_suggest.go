package sihttp

import "encoding/json"

// GroupSuggestSchemaJSON is the JSON Schema enforced on group
// suggestion output. Mirrors the fields BuildGroupSuggestMessages asks
// the model for.
var GroupSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "description", "suggestedMemberIds"],
  "properties": {
    "name": {"type": "string", "description": "Short team/department name, under 4 words, no suffixes like Workspace or Group."},
    "description": {"type": "string", "description": "1-2 sentences explaining the group's purpose."},
    "suggestedMemberIds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Array of user IDs from the available list. Empty when none match."
    }
  }
}`)

// GroupUser describes a user available for group-creation suggestions.
type GroupUser struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Role        string `json:"role"`
}
