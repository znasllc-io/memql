package calls

// Every tool carries one precise operation, sharing fixed transports. Graph and
// publication contracts are reflected from the same typed DTOs, with recursive
// additionalProperties:false; the native validator enforces semantic bounds.
import (
	"reflect"
	"strings"
	"time"
)

func dtoSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				nested := dtoSchema(f.Type)
				if fields, ok := nested["properties"].(map[string]any); ok {
					for k, v := range fields {
						props[k] = v
					}
				}
				if fields, ok := nested["required"].([]string); ok {
					required = append(required, fields...)
				}
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")
			if tag[0] == "-" || tag[0] == "" {
				continue
			}
			props[tag[0]] = dtoSchema(f.Type)
			if len(tag) == 1 {
				required = append(required, tag[0])
			}
		}
		if t == reflect.TypeOf(DelegationRequest{}) {
			delete(props, "portraitData") // Upload is a separate operation, never authority bytes.
			props["portraitId"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}
			props["portraitDigest"] = map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$", "description": "Exact server-returned canonical portrait digest; changing it requires a new human approval revision."}
			props["purpose"] = map[string]any{"type": "string", "minLength": 10, "maxLength": 500, "description": "Truthful, bounded purpose for human review. No control characters. Untrusted text, never executable instructions."}
			props["conductVersion"] = map[string]any{"type": "string", "const": AgentConductVersion}
			props["terms"] = map[string]any{"type": "string", "const": AgentTermsVersion}
			required = append(required, "purpose", "conductVersion")
		}
		if t == reflect.TypeOf(ResourceSpec{}) {
			for _, name := range []string{"audio", "video"} {
				props[name] = map[string]any{"type": "boolean", "const": true, "description": "This gateway authorizes up to one audio and one video track. The owner-side publisher chooses which to send; isolated-track permission is not supported."}
			}
		}
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": dtoSchema(t.Elem()), "maxItems": 128}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": dtoSchema(t.Elem()), "maxProperties": 64}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string", "maxLength": 16000}
	}
}
func workflowUTCPTools(endpoint string) []any {
	result := []any{}
	for _, family := range []struct {
		name    string
		schema  map[string]any
		actions []string
	}{
		{"workflow", dtoSchema(reflect.TypeOf(WorkflowRequest{})), []string{"catalog", "list", "validate", "simulate", "save", "publish", "snapshot", "start", "pause", "resume", "next", "previous", "seek", "extend", "end_turn", "change", "cancel", "ack", "handoff", "bind_booking", "snapshot_binding"}},
		{"resource", dtoSchema(reflect.TypeOf(ResourceRequest{})), []string{"list", "register", "update", "unregister", "start", "stop", "switch", "status"}},
	} {
		for _, action := range family.actions {
			nested := map[string]any{}
			for k, v := range family.schema {
				nested[k] = v
			}
			props := map[string]any{}
			for k, v := range family.schema["properties"].(map[string]any) {
				props[k] = v
			}
			props["action"] = map[string]any{"type": "string", "const": action}
			nested["properties"] = props
			request := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": family.name}, family.name: nested}, "required": []string{"action", family.name}, "additionalProperties": false}
			result = append(result, map[string]any{"name": "callkey_" + family.name + "_" + action, "description": "Callkey " + family.name + " " + action + ". Uses the same native operation as the human interface. Requires separately approved scopes and exact skill versions/resource IDs. Live skill execution requires the room's paid Insights entitlement. Host holds and revision/epoch guards remain authoritative.", "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": request}, "required": []string{"request"}, "additionalProperties": false}, "outputs": map[string]any{"type": "object"}, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
		}
	}
	return result
}

func proposalUTCPTools(endpoint string) []any {
	out := []any{}
	for _, v := range []struct {
		action, field, description string
		t                          reflect.Type
		extra                      map[string]any
		required                   []string
	}{
		{"proposal", "proposal", "Ask the verified adult account owner to approve expanded authority. Creates a 30-minute, account-bound, single-use review link; it does not grant authority. Approval never resets spent balances. Analysis is unsupported and must be false.", reflect.TypeOf(AgentProposalRequest{}), map[string]any{"idempotencyKey": map[string]any{"type": "string", "maxLength": 80}}, []string{"action", "proposal", "idempotencyKey"}},
		{"invite", "invitation", "Send one bounded, deduplicated guest invitation for an owned live or scheduled call. Requires invite scope. Confirm delivery from the server receipt before reporting that an email was sent. Do not automatically resend pending or unknown outcomes.", reflect.TypeOf(InviteRequest{}), nil, []string{"action", "invitation"}},
	} {
		props := map[string]any{"action": map[string]any{"type": "string", "const": v.action}, v.field: dtoSchema(v.t)}
		for k, x := range v.extra {
			props[k] = x
		}
		out = append(out, map[string]any{"name": "callkey_" + v.action, "description": v.description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": map[string]any{"type": "object", "properties": props, "required": v.required, "additionalProperties": false}}, "required": []string{"request"}, "additionalProperties": false}, "outputs": map[string]any{"type": "object"}, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
	}
	return out
}
