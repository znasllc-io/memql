package calls

import "reflect"

func messageUTCPTool(endpoint string) any {
	variants := []any{}
	for _, kind := range []string{"chat", "reaction", "hand"} {
		fields := map[string]any{"roomId": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}, "identity": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}, "sequence": map[string]any{"type": "integer", "minimum": 1, "maximum": 9007199254740990}, "kind": map[string]any{"const": kind}}
		required := []string{"roomId", "identity", "sequence", "kind"}
		switch kind {
		case "chat":
			fields["text"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 2000, "description": "At most 2000 UTF-16 units and 8000 UTF-8 bytes; plain text only."}
			required = append(required, "text")
		case "reaction":
			fields["reaction"] = map[string]any{"type": "string", "maxLength": 64, "description": "Exact identifier from same-origin /reaction-identifiers.json; verify its SHA-256 against session.messages.reactionCatalogDigest."}
			required = append(required, "reaction")
		case "hand":
			fields["raised"] = map[string]any{"type": "boolean"}
			required = append(required, "raised")
		}
		variants = append(variants, map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false})
	}
	request := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"const": "message"}, "message": map[string]any{"oneOf": variants}}, "required": []string{"action", "message"}, "additionalProperties": false}
	description := "Send chat, a catalog reaction or your own raised-hand state with explicit message scope. " +
		"Requires session.messages.available and exact nextSequence/current roomId/identity. Native relay sets sender attribution. " +
		"Chat 1/second, reaction 1/second, hand 1/400 ms; shared room/backend limits may refuse sooner. " +
		"Retry ambiguous requests with identical sequence and body only. duplicate means a prior send attempt, not confirmed delivery; never automatically resend under a new sequence. " +
		"Refresh session before a new intent after stale or abandoned work. Receive only authenticated server packets on callkey.application.v1, deduplicate eventId. " +
		"No history endpoint or content retention. New relay-room tokens cannot publish peer data; old issued tokens are not retroactively revoked."
	output := dtoSchema(reflect.TypeOf(MessageResult{}))
	output["properties"].(map[string]any)["status"] = map[string]any{"type": "string", "enum": []string{"published", "duplicate", "stale", "rate_limited", "unavailable", "delivery_uncertain"}}
	return map[string]any{"name": "callkey_message", "description": description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": request}, "required": []string{"request"}, "additionalProperties": false}, "outputs": output, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}}
}
