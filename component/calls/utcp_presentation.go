package calls

import "reflect"

func presentationUTCPTools(endpoint string) []any {
	out := []any{}
	for _, action := range []string{"presentation_status", "presentation_set", "presentation_clear"} {
		props := map[string]any{"action": map[string]any{"type": "string", "const": action}}
		required := []string{"action"}
		description := "Read native presentation ownership, audience publication permissions and enforcement for the current Insights Skills visit. Requires the exact approved skill version; family calls are excluded. Guest projections name the presenter/speaker but never expose host-only choices. Media publication permissions never turn on a device, unmute, grant subscription or authorize observations."
		if action == "presentation_status" {
			description += " Read status requires skills_read."
		}
		if action != "presentation_status" {
			props["runId"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 80}
			props["expectedRevision"] = map[string]any{"type": "integer", "minimum": 1}
			props["expectedEpoch"] = map[string]any{"type": "integer", "minimum": 1}
			props["idempotencyKey"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$"}
			fields := map[string]any{"visitId": map[string]any{"type": "string", "minLength": 1, "maxLength": 80}, "expectedRevision": map[string]any{"type": "integer", "minimum": 0}}
			nestedRequired := []string{"visitId", "expectedRevision"}
			if action == "presentation_set" {
				fields["presenterSeatId"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "description": "An exact current host-projected choice, including a managed resource ingress's native seat."}
				fields["audience"] = dtoSchema(reflect.TypeOf(PublicationPermissions{}))
				fields["speakerSeatId"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128}
				fields["speakerSeconds"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 300, "description": "Required with speakerSeatId. Temporary microphone publication permission only; never unmutes."}
				nestedRequired = append(nestedRequired, "presenterSeatId", "audience")
			}
			props["presentation"] = map[string]any{"type": "object", "properties": fields, "required": nestedRequired, "additionalProperties": false}
			required = append(required, "runId", "expectedRevision", "expectedEpoch", "idempotencyKey", "presentation")
			description += " Changing or clearing requires an authorized host, BOTH skills_control and control, exact run/visit/revisions/epoch and no human control hold. Read status first. Retry the identical intent and key after an unknown response; never infer provider enforcement from a saved intent. pending must be reconciled before claiming applied. Screen-share audio follows screen-share permission."
		}
		workflow := map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
		request := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": "workflow"}, "workflow": workflow}, "required": []string{"action", "workflow"}, "additionalProperties": false}
		out = append(out, map[string]any{"name": "callkey_workflow_" + action, "description": description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": request}, "required": []string{"request"}, "additionalProperties": false}, "outputs": map[string]any{"type": "object", "properties": map[string]any{"presentation": dtoSchema(reflect.TypeOf(PresentationStatus{})), "serverNow": map[string]any{"type": "string", "format": "date-time"}}, "required": []string{"presentation", "serverNow"}, "additionalProperties": false}, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
	}
	return out
}
