package calls

import "reflect"

func observationUTCPTools(endpoint string) []any {
	out := []any{}
	for _, action := range []string{"observations", "observation_request", "observation_consent", "observation_withdraw", "observation_publish"} {
		fields := map[string]any{"roomId": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{1,128}$"}, "identity": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{1,128}$"}}
		required := []string{"roomId", "identity"}
		description := "Requires explicit observations scope and current approved admission in an adult Insights call. Shares bounded untrusted text observations only: no media token, screenshot feed, device access, trusted processing authority, age proof or automatic sanction. Existing room_media access is separate and is not narrowed by this consent. Read current permissions first; only exact approved recipient admissions receive server-origin packets on callkey.observations.v1. No stored observation text or history."
		if action != "observations" {
			fields["idempotencyKey"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$"}
			required = append(required, "idempotencyKey")
			description += " Retry identical intent and key only. duplicate records a previous send attempt, not confirmed delivery; never silently resend under a new key."
		}
		if action == "observation_request" {
			fields["subjectIdentity"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{1,128}$"}
			fields["recipientIdentities"] = map[string]any{"type": "array", "minItems": 2, "maxItems": 8, "uniqueItems": true, "items": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{1,128}$"}}
			fields["purpose"] = map[string]any{"type": "string", "minLength": 10, "maxLength": 500, "description": "For an agent, must equal its exact human-approved grant purpose."}
			fields["durationSeconds"] = map[string]any{"type": "integer", "minimum": 60, "maximum": 300}
			fields["notice"] = map[string]any{"type": "string", "const": ObservationNotice}
			required = append(required, "subjectIdentity", "recipientIdentities", "purpose", "durationSeconds", "notice")
			description += " Recipients must include the requester and subject. Only the subject can approve, and host status cannot substitute for consent. New purpose/recipients/admissions require a new approval. At most four open requests per Callkey, 64 per room, one request per 10 seconds."
		} else if action != "observations" {
			fields["id"] = map[string]any{"type": "string", "maxLength": 80}
			fields["revision"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 3}
			required = append(required, "id", "revision")
			if action == "observation_consent" {
				fields["notice"] = map[string]any{"type": "string", "const": ObservationNotice}
				required = append(required, "notice")
				description += " Only the subject's current admission may approve this exact purpose, observer, recipients and expiry."
			}
			if action == "observation_withdraw" {
				description += " Subject or requester can withdraw; withdrawal remains available after Insights/expiry loss. Previously delivered text cannot be recalled."
			}
			if action == "observation_publish" {
				fields["text"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 500}
				required = append(required, "text")
				description += " Only the approved requester may publish: at most 20 observations per permission and one per 10 seconds per Callkey. All recipients must still be authorized; one changed admission refuses the whole send."
			}
		}
		nested := map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false}
		workflow := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": action}, "observation": nested}, "required": []string{"action", "observation"}, "additionalProperties": false}
		request := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": "workflow"}, "workflow": workflow}, "required": []string{"action", "workflow"}, "additionalProperties": false}
		out = append(out, map[string]any{"name": "callkey_workflow_" + action, "description": description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": request}, "required": []string{"request"}, "additionalProperties": false}, "outputs": map[string]any{"type": "object", "properties": map[string]any{"observations": dtoSchema(reflect.TypeOf(Observations{})), "observationReceipt": dtoSchema(reflect.TypeOf(ObservationReceipt{})), "serverNow": map[string]any{"type": "string", "format": "date-time"}}, "required": []string{"observations", "serverNow"}, "additionalProperties": false}, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
	}
	return out
}
