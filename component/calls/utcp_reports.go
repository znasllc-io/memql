package calls

import "reflect"

func reportUTCPTools(endpoint string) []any {
	tools := []any{}
	for _, action := range []string{"report_submit", "report_status", "report_list", "report_appeal"} {
		fields := map[string]any{}
		all := map[string]any{
			"roomId":         map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"identity":       map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"targetIdentity": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
			"category":       map[string]any{"type": "string", "enum": []string{"harassment", "sexual_content", "child_safety", "impersonation", "spam", "other"}},
			"description":    map[string]any{"type": "string", "minLength": 10, "maxLength": 2000, "description": "Plain untrusted text describing an observation; no media, attachments, identity inference or secret data."},
			"idempotencyKey": map[string]any{"type": "string", "pattern": "^[A-Za-z0-9_-]{16,128}$"},
			"id":             map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
			"revision":       map[string]any{"type": "integer", "minimum": 1, "maximum": 4},
			"cursor":         map[string]any{"type": "string", "maxLength": 4096},
		}
		names := map[string][]string{"report_submit": {"roomId", "identity", "targetIdentity", "category", "description", "idempotencyKey"}, "report_status": {"id"}, "report_list": {"cursor"}, "report_appeal": {"id", "revision", "description", "idempotencyKey"}}[action]
		required := []string{}
		for _, name := range names {
			fields[name] = all[name]
			if name != "cursor" && name != "targetIdentity" {
				required = append(required, name)
			}
		}
		description := map[string]string{
			"report_submit": "Submit bounded metadata about your current admitted call under explicitly human-approved report scope. Read session.reports first. Send exact current roomId and your own identity; optional targetIdentity must be a current peer. No screenshots, recordings, attachments, age guesses or agent-generated findings. A report is an allegation, not a finding; it does not automatically sanction anyone or contact external authorities. Reuse the same idempotency key and exact body on uncertain retry. Five submissions per Callkey per rolling24h, with shared room/site budgets. Metadata expires at the server receipt's immutable expiresAt.",
			"report_status": "Read only this grant's own submitted receipt and neutral review outcome. It reveals no reviewer notes or other participant's reports. Missing, expired and unauthorized IDs have the same refusal. action_recommended is a review recommendation, not proof an account sanction or external report occurred.",
			"report_list":   "Read at most25 own-grant report receipts with an opaque nextCursor. Never enumerate another grant or human owner's submissions, even when they share a Callkey. Status remains available if new submissions are disabled, until immutable expiry.",
			"report_appeal": "Ask for one further review of your own reviewed report. Send the exact receipt revision and a stable idempotency key. One appeal only; review does not extend metadata expiry. This is case metadata review, not a legal appeal or automatic sanction.",
		}[action]
		report := map[string]any{"type": "object", "properties": fields, "required": required, "additionalProperties": false}
		request := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": action}, "report": report}, "required": []string{"action", "report"}, "additionalProperties": false}
		tools = append(tools, map[string]any{"name": "callkey_" + action, "description": description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": request}, "required": []string{"request"}, "additionalProperties": false}, "outputs": dtoSchema(reflect.TypeOf(ReportResult{})), "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
	}
	return tools
}
