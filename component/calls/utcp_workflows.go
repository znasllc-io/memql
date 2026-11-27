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
			props["scopes"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": DelegationScopes}, "minItems": 1, "maxItems": len(DelegationScopes), "uniqueItems": true, "description": "Explicit owner-approved capabilities. questions and observations are separate from ephemeral message and room_media permission; none is added implicitly. observations authorizes consented untrusted text forwarding only, never media access."}
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
		if t == reflect.TypeOf(PresentationChoice{}) {
			props["kind"] = map[string]any{"type": "string", "enum": []string{"person", "agent", "avatar", "resource"}}
		}
		if t == reflect.TypeOf(ObservationPerson{}) {
			props["kind"] = map[string]any{"type": "string", "enum": []string{"person", "agent", "avatar"}}
		}
		if t == reflect.TypeOf(WorkflowRepeatSpec{}) {
			props["count"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 8, "description": "Finite sequential rounds. Every path reaches the inclusive untilNodeId exit; rounds require fresh acknowledgments and never replay effects automatically."}
			props["untilNodeId"] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$"}
		}
		if t == reflect.TypeOf(WorkflowRepeatProgress{}) {
			for _, field := range []string{"count", "iteration"} {
				props[field] = map[string]any{"type": "integer", "minimum": 1, "maximum": 8}
			}
		}
		if t == reflect.TypeOf(WorkflowSimulation{}) {
			props["maximumVisits"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 256}
			props["nodeIds"].(map[string]any)["maxItems"] = 256
		}
		if t == reflect.TypeOf(WorkflowRun{}) || t == reflect.TypeOf(WorkflowParallelBranch{}) {
			props["history"].(map[string]any)["maxItems"] = 256
			props["seekableNodeIds"].(map[string]any)["maxItems"] = 64
			props["acknowledgedSeatIds"].(map[string]any)["maxItems"] = 100
		}
		if t == reflect.TypeOf(WorkflowRun{}) {
			props["events"].(map[string]any)["maxItems"] = 256
		}
		if t == reflect.TypeOf(WorkflowParallelProgress{}) {
			props["branches"].(map[string]any)["minItems"] = 2
			props["branches"].(map[string]any)["maxItems"] = 4
		}
		if t == reflect.TypeOf(WorkflowParallelBranch{}) {
			props["status"] = map[string]any{"type": "string", "enum": []string{"running", "blocked", "joined"}}
			props["allowedActions"] = map[string]any{"type": "array", "maxItems": 6, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"next", "previous", "seek", "extend", "ack", "end_turn"}}, "description": "Authoritative current branch controls. A joined branch has no actions. Global pause, resume, cancel and handoff act on the whole run."}
		}
		if t == reflect.TypeOf(WorkflowDefinition{}) {
			props["nodes"].(map[string]any)["maxItems"] = 64
			props["roles"].(map[string]any)["maxItems"] = 16
		}
		if t == reflect.TypeOf(WorkflowParallelSpec{}) {
			props["branches"] = map[string]any{"type": "array", "items": dtoSchema(reflect.TypeOf(WorkflowParallelBranchSpec{})), "minItems": 2, "maxItems": 4, "description": "Two to four disjoint guidance branches. Controller edges exactly match their entries; every inclusive exit has one edge to the same join-all successor. No nested repeat/parallel, presentation, resource ownership or effects inside branches."}
		}
		if t == reflect.TypeOf(WorkflowParallelBranchSpec{}) {
			for _, field := range []string{"id", "entryNodeId", "untilNodeId"} {
				props[field] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$"}
			}
		}
		if t == reflect.TypeOf(WorkflowEffectSpec{}) {
			props["action"] = map[string]any{"type": "string", "const": "end_turn_mute", "description": "A host or explicitly approved agent must review and invoke the existing end_turn_mute operation for this exact visit. Entry, resume and revisit never execute it. No arbitrary effects or device start are supported."}
		}
		if t == reflect.TypeOf(WorkflowNode{}) {
			props["type"] = map[string]any{"type": "string", "enum": []string{"readiness", "instruction", "turn", "discussion", "presentation", "acknowledgment", "condition", "handoff", "repeat", "parallel", "effect", "finish"}, "description": "Repeat and parallel are bounded structured regions. Effect requires roleId, zero duration, exactly one successor and the closed end_turn_mute action."}
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
		{"workflow", dtoSchema(reflect.TypeOf(WorkflowRequest{})), []string{"catalog", "list", "validate", "simulate", "save", "publish", "snapshot", "start", "pause", "resume", "next", "previous", "seek", "extend", "end_turn", "end_turn_mute", "change", "cancel", "ack", "handoff", "bind_booking", "snapshot_binding"}},
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
			delete(props, "question")     // Questions have their own closed operation-specific schema.
			delete(props, "observation")  // Consent and untrusted text forwarding are separately scoped.
			delete(props, "presentation") // Presentation actions have a closed revision-bound contract.
			if family.name == "workflow" {
				if action == "next" || action == "previous" || action == "seek" || action == "extend" || action == "ack" || action == "end_turn" {
					for _, field := range []string{"branchId", "branchVisitId"} {
						props[field] = map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$", "description": "Provide both branchId and its exact current branchVisitId for a parallel branch command, alongside the shared run revision/epoch. Omit both for outer guidance. Branch completion waits at join-all; it never advances a sibling."}
					}
					nested["dependentRequired"] = map[string]any{"branchId": []string{"branchVisitId"}, "branchVisitId": []string{"branchId"}}
				} else {
					delete(props, "branchId")
					delete(props, "branchVisitId")
				}
			}
			props["action"] = map[string]any{"type": "string", "const": action}
			if action == "end_turn_mute" {
				props = map[string]any{
					"action":           map[string]any{"type": "string", "const": action},
					"runId":            map[string]any{"type": "string", "minLength": 1, "maxLength": 80},
					"expectedRevision": map[string]any{"type": "integer", "minimum": 1},
					"expectedEpoch":    map[string]any{"type": "integer", "minimum": 1},
					"idempotencyKey":   map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,79}$"},
					"nodeId":           map[string]any{"type": "string", "maxLength": 80, "description": "Explicit legal next node when the current turn has multiple outgoing branches."},
				}
				nested["required"] = []string{"action", "runId", "expectedRevision", "expectedEpoch", "idempotencyKey"}
			}
			nested["properties"] = props
			request := map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "const": family.name}, family.name: nested}, "required": []string{"action", family.name}, "additionalProperties": false}
			description := "Callkey " + family.name + " " + action + ". Uses the same native operation as the human interface. Requires separately approved scopes and exact skill versions/resource IDs. Live skill execution requires the room's paid Insights entitlement. Host holds and revision/epoch guards remain authoritative."
			if action == "end_turn_mute" {
				description += " Host only; requires BOTH skills_control and control. Explicitly review the assigned participant on the current turn or closed effect node before invocation. One durable intent pins that exact visit, participant and microphone publications. Retry the identical request and idempotency key: pending or uncertain retries only reconcile, never mute again. A completed effect confirms advancement; cancelled means no advancement, but already dispatched microphones may have been muted. Entry, resume and revisit never authorize automatic execution. Parallel branch effects are unavailable. Never unmute automatically."
			}
			result = append(result, map[string]any{"name": "callkey_" + family.name + "_" + action, "description": description, "inputs": map[string]any{"type": "object", "properties": map[string]any{"request": request}, "required": []string{"request"}, "additionalProperties": false}, "outputs": map[string]any{"type": "object"}, "tool_call_template": map[string]any{"call_template_type": "http", "url": endpoint, "http_method": "POST", "content_type": "application/json", "body_field": "request", "auth": map[string]any{"auth_type": "api_key", "api_key": "Bearer ${CALLKEY_AGENT_TOKEN}", "var_name": "Authorization", "location": "header"}}})
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
