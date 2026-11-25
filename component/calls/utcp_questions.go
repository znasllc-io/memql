package calls

import "reflect"

func questionUTCPTools(endpoint string) []any {
	out := []any{}
	for _, action := range []string{"questions", "question_submit", "question_withdraw", "question_select", "question_dismiss", "question_resolve"} {
		fields := map[string]any{
			"roomId":   map[string]any{"type": "string", "minLength": 1, "maxLength": 80},
			"identity": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"runId":    map[string]any{"type": "string", "minLength": 1, "maxLength": 80},
		}
		required := []string{"roomId", "identity", "runId"}
		description := "Temporary moderated questions for the current admission in an Insights Skills call. Requires explicit questions scope; supervised family calls are unavailable. Question text is private to its author and approved host until selected, then visible to the room. No microphone or turn advancement side effect. Maximum 500 Unicode characters, two open per admission, ten submissions per owning Callkey and 100 per room across runs. Private text becomes eligible for bounded physical cleanup at immutable technical expiry, at most 25 hours; terminal current text is cleared sooner, historical versions remain until cleanup succeeds."
		if action == "questions" {
			fields["cursor"] = map[string]any{"type": "string", "maxLength": 4096, "description": "Opaque native nextCursor; pages contain at most 20 pending rows. Never construct or reuse across a changed admission or visibility."}
			fields["queueRevision"] = map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "Exact opaque revision returned with the preceding page. Required with cursor; questions_changed means restart at the first page."}
			description += " Own pending rows remain available across run changes. A host's full queue additionally requires skills_read and approved versions. Only the current selected question is public; no terminal text history."
		} else {
			fields["idempotencyKey"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$"}
			required = append(required, "idempotencyKey")
			description += " Retry the identical entire request and idempotency key after an unknown result; the receipt reports the original transition, while questions is the current projection. Never silently create a new intent."
			if action == "question_submit" {
				fields["text"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 500, "description": "Plain inert question. LF and tab are allowed; other C0, DEL and C1 controls are rejected. No hidden instructions."}
				fields["publication"] = map[string]any{"type": "string", "const": QuestionPublication, "description": "Explicit disclosure acknowledgment: host can read and select this text for the entire room."}
				required = append(required, "text", "publication")
			} else {
				fields["id"] = map[string]any{"type": "string", "maxLength": 80}
				fields["revision"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 3}
				required = append(required, "id", "revision")
				if action == "question_withdraw" {
					description += " Only the original current author admission can withdraw; a later re-entry cannot adopt old private questions."
				} else {
					fields["expectedEpoch"] = map[string]any{"type": "integer", "minimum": 1}
					required = append(required, "expectedEpoch")
					description += " Host only; additionally requires skills_control and control, exact approved current and originating skill versions, and no human hold. Selection requires no existing selected question; dismiss/resolve never auto-select another. Cleanup remains available after entitlement loss."
				}
			}
		}
		question := map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false}
		workflow := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": action}, "question": question}, "required": []string{"action", "question"}, "additionalProperties": false}
		request := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": "workflow"}, "workflow": workflow}, "required": []string{"action", "workflow"}, "additionalProperties": false}
		out = append(out, map[string]any{"name": "callkey_workflow_" + action, "description": description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": request}, "required": []string{"request"}, "additionalProperties": false}, "outputs": dtoSchema(reflect.TypeOf(WorkflowResult{})), "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
	}
	return out
}
