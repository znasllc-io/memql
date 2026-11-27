package calls

import (
	"strings"
	"testing"
)

func TestObservationContractRequiresExactBoundedDisclosure(t *testing.T) {
	q := ObservationRequest{RoomID: "room", Identity: "observer", SubjectIdentity: "subject", RecipientIdentities: []string{"observer", "subject"}, Purpose: "Review the discussion contributions", DurationSeconds: 300, Notice: ObservationNotice, IdempotencyKey: "request-one"}
	r := WorkflowRequest{Action: "observation_request", Observation: &q}
	if !ValidObservationRequest(r) {
		t.Fatal("valid request refused")
	}
	for _, mutate := range []func(*ObservationRequest){func(q *ObservationRequest) { q.Notice = "" }, func(q *ObservationRequest) { q.DurationSeconds = 301 }, func(q *ObservationRequest) { q.RecipientIdentities = []string{"observer", "observer"} }, func(q *ObservationRequest) { q.RecipientIdentities = []string{"observer", "other"} }, func(q *ObservationRequest) { q.Text = "mixed operation" }, func(q *ObservationRequest) { q.Purpose = "hidden\x00instructions" }, func(q *ObservationRequest) { q.SubjectIdentity = q.Identity }} {
		copy := q
		mutate(&copy)
		if ValidObservationRequest(WorkflowRequest{Action: r.Action, Observation: &copy}) {
			t.Fatal("unbounded or mixed intent accepted")
		}
	}
	r.Question = &QuestionRequest{}
	if ValidObservationRequest(r) {
		t.Fatal("mixed Questions authority accepted")
	}
	for _, text := range []string{"", strings.Repeat("界", 501), "text\u0085", " padded "} {
		p := ObservationRequest{RoomID: "room", Identity: "observer", ID: "permission", Revision: 2, IdempotencyKey: "publish", Text: text}
		if ValidObservationRequest(WorkflowRequest{Action: "observation_publish", Observation: &p}) {
			t.Fatal("invalid observation accepted")
		}
	}
}

func TestObservationManualKeepsScopeSeparateFromMediaAuthority(t *testing.T) {
	manual := UTCPManual("https://example.test/_memql/calls/agent")
	if manual["manual_version"] != "2.8.0" || CurrentAgentPolicy().Media.Subscribe != "room_media" {
		t.Fatal("additive version or existing media policy changed")
	}
	count := 0
	for _, raw := range manual["tools"].([]any) {
		tool := raw.(map[string]any)
		if !strings.HasPrefix(tool["name"].(string), "callkey_workflow_observation") {
			continue
		}
		count++
		description := tool["description"].(string)
		if !strings.Contains(description, "explicit observations scope") || !strings.Contains(description, "no media token") {
			t.Fatal("observation/media boundary missing")
		}
		request := tool["inputs"].(map[string]any)["properties"].(map[string]any)["request"].(map[string]any)
		workflow := request["properties"].(map[string]any)["workflow"].(map[string]any)
		if len(workflow["properties"].(map[string]any)) != 2 {
			t.Fatal("generic operations mixed into observation tool")
		}
	}
	if count != 5 {
		t.Fatalf("got %d observation tools", count)
	}
}
