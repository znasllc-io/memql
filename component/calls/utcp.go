package calls

import "reflect"

// UTCPManual describes the native HTTP operations. Arguments cannot select a
// different URL, secret or engine operation. The server enforces every bound.
func UTCPManual(endpoint string) map[string]any {
	tools := []any{}
	actions := []string{"authority", "balance", "quote", "start", "schedule", "reschedule", "cancel", "session", "history", "host", "leave", "topup", "join", "request_join", "join_status", "guest_join", "invitation_link", "rename", "archive_preview", "archive", "restore", "proposal_status", "portrait_upload", "portrait_get", "processing_consent"}
	for _, action := range actions {
		props := map[string]any{"action": map[string]any{"type": "string", "const": action}}
		all := map[string]any{}
		for _, name := range []string{"idempotencyKey", "quoteId", "title", "name", "terms", "participation", "start", "callId", "control", "seat", "confirmationToken", "proposalId", "guestToken", "requestId"} {
			all[name] = map[string]any{"type": "string", "maxLength": 128}
		}
		all["title"] = map[string]any{"type": "string", "maxLength": 80}
		all["name"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 64}
		all["idempotencyKey"] = map[string]any{"type": "string", "minLength": 16, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}
		all["guestToken"] = map[string]any{"type": "string", "pattern": "^[a-z2-7]{32}$"}
		all["requestId"] = map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}
		all["start"] = map[string]any{"type": "string", "format": "date-time"}
		all["terms"] = map[string]any{"type": "string", "const": AgentTermsVersion}
		all["participation"] = map[string]any{"type": "string", "const": "adults_only"}
		all["participantLimit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
		all["durationMinutes"] = map[string]any{"type": "integer", "oneOf": []any{map[string]any{"const": 10}, map[string]any{"minimum": 15, "maximum": 1440, "multipleOf": 15}}}
		for _, name := range []string{"insights", "resources", "authorityAccepted", "acknowledged"} {
			all[name] = map[string]any{"type": "boolean"}
		}
		all["acceptance"] = agentAcceptanceSchema()
		all["portraitId"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}
		all["processing"] = dtoSchema(reflect.TypeOf(ProcessingConsentRequest{}))
		all["portraitData"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 54616, "contentEncoding": "base64", "description": "PNG/JPEG bytes only, no data URL. Maximum 40,960 decoded source bytes; each dimension 1–512px. Native service strips metadata and reencodes."}
		fields := map[string][]string{
			"processing_consent": {"processing"}, "portrait_upload": {"portraitData"}, "portrait_get": {"portraitId"},
			"quote":      {"participantLimit", "durationMinutes", "insights", "resources"},
			"start":      {"idempotencyKey", "quoteId", "title", "name", "terms", "participation", "authorityAccepted", "acceptance"},
			"schedule":   {"idempotencyKey", "quoteId", "title", "start", "acceptance"},
			"reschedule": {"idempotencyKey", "quoteId", "title", "start", "callId", "acceptance"},
			"cancel":     {"callId"}, "host": {"control", "seat"}, "topup": {"idempotencyKey", "acceptance"},
			"join":         {"callId", "name", "idempotencyKey", "acceptance"},
			"request_join": {"guestToken", "name", "idempotencyKey"}, "join_status": {"callId", "requestId"},
			"guest_join": {"guestToken", "callId", "requestId", "idempotencyKey", "acceptance"},
			"rename":     {"callId", "title"}, "archive_preview": {"callId"}, "archive": {"callId", "confirmationToken", "acknowledged"}, "restore": {"callId"}, "proposal_status": {"proposalId"},
		}[action]
		for _, name := range fields {
			props[name] = all[name]
		}
		required := []string{"action"}
		required = append(required, map[string][]string{
			"processing_consent": {"processing"}, "portrait_upload": {"portraitData"}, "portrait_get": {"portraitId"},
			"quote": {"participantLimit", "durationMinutes"}, "start": {"idempotencyKey", "quoteId", "name", "acceptance"},
			"schedule": {"idempotencyKey", "quoteId", "start", "acceptance"}, "reschedule": {"idempotencyKey", "quoteId", "start", "callId", "acceptance"},
			"cancel": {"callId"}, "host": {"control"}, "topup": {"idempotencyKey", "acceptance"}, "join": {"callId", "name", "idempotencyKey", "acceptance"},
			"request_join": {"guestToken", "name", "idempotencyKey"}, "join_status": {"callId", "requestId"}, "guest_join": {"guestToken", "callId", "requestId", "idempotencyKey", "acceptance"},
			"rename": {"callId", "title"}, "archive_preview": {"callId"}, "archive": {"callId", "confirmationToken", "acknowledged"}, "restore": {"callId"}, "proposal_status": {"proposalId"},
		}[action]...)
		description := "Callkey " + action + " under the authenticated grant's separately approved scope and limits. Never infer host privileges from a facilitator role."
		output := map[string]any{"type": "object"}
		switch action {
		case "processing_consent":
			description = "Explicitly opt only this agent admission into optional trusted processing under separately human-approved processing scope. Read session first and send exact roomId, identity and consent revision. Never consent for another participant. Safety checks require native availability and explicit consent; speech transcription and text-tone also require the configured Insights feature. Family calls are excluded. Insights requires transcription. Captions and text-tone are ephemeral and visible to current room participants. Withdraw by setting all flags false; re-entry resets consent. This grants no room monitoring or host privilege."
			output = dtoSchema(reflect.TypeOf(ProcessingStatus{}))
		case "portrait_upload":
			description = "Upload an optional static agent portrait under explicit portraits_write scope. Only bounded PNG/JPEG, no URL/SVG. Returns immutable canonical digest and identifier; upload alone grants no appearance or call authority. Include both portraitId and portraitDigest in a proposal for exact human review. Same canonical bytes deduplicate retries; limit 32 assets per Callkey and 8 new per hour. This sanitizes active content, not visual content."
			output = dtoSchema(reflect.TypeOf(PortraitInfo{}))
		case "portrait_get":
			description = "Read approved canonical image bytes as base64 under read scope. Only own assets, current room participants or eligible pending-review human hosts may read; an asset ID/digest is not authority. Data is at most 1 MiB. Verify SHA256 against approved portraitDigest. Static portraits never represent live camera video."
			output = map[string]any{"type": "object", "properties": map[string]any{"portrait": dtoSchema(reflect.TypeOf(PortraitInfo{})), "data": map[string]any{"type": "string", "maxLength": 1398104, "contentEncoding": "base64"}}, "required": []string{"portrait", "data"}, "additionalProperties": false}
		case "authority":
			description = "Read this grant's immutable approved purpose, exact approval revision/digest and the current policy URL/version/digest. No spending or admission. An old grant requires fresh human approval before new commitments."
			output = dtoSchema(reflect.TypeOf(AgentAuthority{}))
			// Read-only discovery must also describe legacy grants, which are
			// deliberately not silently upgraded to current policy authority.
			grant := output["properties"].(map[string]any)["grant"].(map[string]any)
			grantFields := grant["properties"].(map[string]any)
			grantFields["terms"] = map[string]any{"type": "string", "maxLength": 64}
			grantFields["purpose"] = map[string]any{"type": "string", "maxLength": 500}
			grantFields["conductVersion"] = map[string]any{"type": "string", "maxLength": 64}
			requiredGrant := []string{}
			for _, field := range grant["required"].([]string) {
				if field != "purpose" && field != "conductVersion" {
					requiredGrant = append(requiredGrant, field)
				}
			}
			grant["required"] = requiredGrant
		case "start", "schedule", "reschedule", "topup":
			description += " Explicit current terms and conduct acceptance is bound to this exact human approval and idempotent request before any reservation, saved-method charge or media effect. Reuse the same key and exact request for ambiguous retries."
		case "join":
			description = "Join a scheduled call owned by this grant as its host. Fresh admission requires exact current acceptance and a new idempotency key. Reconnect may reuse the same occupying admission; a left admission cannot be replayed. Use request_join for another host's guest invitation."
			output = agentJoinOutputSchema()
		case "request_join":
			description = "Request guest admission to an open adults-only configured call. Requires join scope and a current human-approved purpose. This creates no media or spend; an eligible human host must review even if unlocked. Review expires in 10 minutes, earlier if the call ends. Declared purpose is untrusted text, not a verified intent verdict."
			output = dtoSchema(reflect.TypeOf(AgentJoinStatus{}))
		case "join_status":
			description = "Read only this grant's room-bound guest admission request. Poll moderately. pending requires an eligible verified human host to approve; rejected/expired needs a new request, never an automatic bypass."
			output = dtoSchema(reflect.TypeOf(AgentJoinStatus{}))
		case "guest_join":
			description = "After explicit human-host approval, accept current terms and conduct for this exact grant approval and fresh guest admission. Never gains host privileges. One admission only; ambiguous retries may reconnect its still-present seat. A departed or uncertain admission requires new host review. Family calls are excluded."
			output = agentJoinOutputSchema()
		}
		tools = append(tools, map[string]any{"name": "callkey_" + action, "description": description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}, "required": []string{"request"}, "additionalProperties": false}, "outputs": output, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
	}
	tools = append(tools, reportUTCPTools(endpoint)...)
	tools = append(tools, workflowUTCPTools(endpoint)...)
	tools = append(tools, proposalUTCPTools(endpoint)...)
	return map[string]any{"utcp_version": "1.1.0", "manual_version": "2.3.0", "agent_policy": CurrentAgentPolicy(), "tools": tools}
}
func agentAcceptanceSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"approvalRevision": map[string]any{"type": "integer", "minimum": 1}, "approvalDigest": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}, "conductVersion": map[string]any{"type": "string", "const": AgentConductVersion}, "terms": map[string]any{"type": "string", "const": AgentTermsVersion}}, "required": []string{"approvalRevision", "approvalDigest", "conductVersion", "terms"}, "additionalProperties": false}
}
func agentJoinOutputSchema() map[string]any {
	p := map[string]any{"outcome": map[string]any{"type": "string", "enum": []string{"admit", "wait", "queue", "refuse"}}, "host": map[string]any{"type": "boolean"}}
	for _, k := range []string{"url", "pass", "identity", "reason"} {
		p[k] = map[string]any{"type": "string"}
	}
	for _, k := range []string{"expiresInMs", "retryAfterMs", "position"} {
		p[k] = map[string]any{"type": "integer"}
	}
	return map[string]any{"type": "object", "properties": p, "required": []string{"outcome", "host"}, "additionalProperties": false}
}
