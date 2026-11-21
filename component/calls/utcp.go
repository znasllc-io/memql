package calls

// UTCPManual implements the UTCP 1.1 HTTP call-template schema. The request
// object is the body, so invocation arguments cannot become arbitrary URLs or
// caller-selected credentials. JSON schema is discoverability, not enforcement;
// the service independently validates every request and grant.
func UTCPManual(endpoint string) map[string]any {
	tools := []any{}
	actions := []string{"balance", "quote", "start", "schedule", "reschedule", "cancel", "session", "history", "host", "leave", "topup", "join", "invitation_link", "rename", "archive_preview", "archive", "restore", "proposal_status"}
	for _, action := range actions {
		props := map[string]any{"action": map[string]any{"type": "string", "const": action}}
		for _, name := range []string{"idempotencyKey", "quoteId", "title", "name", "terms", "participation", "start", "callId", "control", "seat", "confirmationToken", "proposalId"} {
			props[name] = map[string]any{"type": "string"}
		}
		for _, name := range []string{"participantLimit", "durationMinutes"} {
			props[name] = map[string]any{"type": "integer", "minimum": 1}
		}
		props["insights"] = map[string]any{"type": "boolean"}
		props["resources"] = map[string]any{"type": "boolean"}
		props["authorityAccepted"] = map[string]any{"type": "boolean"}
		props["acknowledged"] = map[string]any{"type": "boolean"}
		tools = append(tools, map[string]any{"name": "callkey_" + action, "description": "Callkey " + action + " under a revocable adult-approved grant; amounts and authority are enforced by the server.", "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": map[string]any{"type": "object", "properties": props, "required": []string{"action"}, "additionalProperties": false}}, "required": []string{"request"}, "additionalProperties": false}, "outputs": map[string]any{"type": "object"}, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
	}
	tools = append(tools, workflowUTCPTools(endpoint)...)
	tools = append(tools, proposalUTCPTools(endpoint)...)
	return map[string]any{"utcp_version": "1.1.0", "manual_version": "1.1.0", "tools": tools}
}
