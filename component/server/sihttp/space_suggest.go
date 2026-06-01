package sihttp

import "encoding/json"

// SpaceSuggestSchemaJSON is the JSON Schema enforced on the space
// suggestion output. Under the one-assistant space model (copresent
// #124) a space has 1+ humans plus exactly one assistant (auto-joined
// by the backend); there is no agent picker and no architecture
// choice, so the suggestion is title-only. The client sends an empty
// agents array and consumes only `title`.
var SpaceSuggestSchemaJSON = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["title"],
  "properties": {
    "title": {"type": "string", "description": "3-7 word space title, no quotes."}
  }
}`)
