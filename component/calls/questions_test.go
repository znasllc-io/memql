package calls

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestQuestionsTypedContractRejectsImplicitDisclosureAndMixedActions(t *testing.T) {
	base := WorkflowRequest{Action: "question_submit", Question: &QuestionRequest{RoomID: "room", Identity: "identity", RunID: "run", Text: "Question?", Publication: QuestionPublication, IdempotencyKey: "ask-1"}}
	if !ValidQuestionRequest(base) {
		t.Fatal("valid submission refused")
	}
	for _, text := range []string{"", strings.Repeat("界", 501), "control\x00", "control\x7f", "control\u0085"} {
		r := base
		q := *base.Question
		r.Question = &q
		q.Text = text
		if ValidQuestionRequest(r) {
			t.Fatalf("accepted invalid bounded text %q", text)
		}
	}
	if !ValidQuestionText("Line one\nLine two\t界") {
		t.Fatal("plain multiline text refused")
	}
	for _, mutate := range []func(*WorkflowRequest){func(r *WorkflowRequest) { r.Question.Publication = "" }, func(r *WorkflowRequest) { r.SkillID = "mixed" }, func(r *WorkflowRequest) { r.Question.Cursor = "cursor" }, func(r *WorkflowRequest) { r.ExpectedEpoch = 1 }} {
		r := base
		q := *base.Question
		r.Question = &q
		mutate(&r)
		if ValidQuestionRequest(r) {
			t.Fatal("mixed/undisclosed request accepted")
		}
	}
}
func TestQuestionsManualKeepsPrivateCapabilityExplicit(t *testing.T) {
	tools := UTCPManual("https://example.test/_memql/calls/agent")["tools"].([]any)
	count := 0
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		if !strings.HasPrefix(name, "callkey_workflow_question") {
			continue
		}
		count++
		description := tool["description"].(string)
		if !strings.Contains(description, "explicit questions scope") {
			t.Fatal("missing scope")
		}
		inputs := tool["inputs"].(map[string]any)
		request := inputs["properties"].(map[string]any)["request"].(map[string]any)
		workflow := request["properties"].(map[string]any)["workflow"].(map[string]any)
		if len(workflow["properties"].(map[string]any)) != 2 {
			t.Fatal("generic authority fields leaked into question action")
		}
	}
	if count != 6 {
		t.Fatalf("expected six precise tools, got %d", count)
	}
}

func TestQuestionsStoredMetadataRejectsUnknownFieldsAndIllegalTransitions(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	state := QuestionStoredState{Author: QuestionAuthor{Name: "Author", Kind: "person", Identity: "identity"}, SeatID: "seat", SessionID: "session", JoinedAt: now.Add(-time.Minute), SkillID: "working-session", SkillVersion: 1, Operations: []QuestionStoredOperation{{Key: "submit", Digest: digest, Actor: digest, BudgetKey: digest, Status: "pending", Revision: 1, At: now}}}
	raw, _ := json.Marshal(state)
	_, valid := ParseQuestionStoredState(string(raw), 1, "pending", now, now.Add(time.Hour))
	if !valid {
		t.Fatal("valid native metadata refused")
	}
	for _, bad := range []string{string(raw) + " {}", strings.TrimSuffix(string(raw), "}") + `,"futureRetentionOverride":true}`, strings.Replace(string(raw), `"BudgetKey":"`+digest+`"`, `"BudgetKey":"bad"`, 1)} {
		if _, ok := ParseQuestionStoredState(bad, 1, "pending", now, now.Add(time.Hour)); ok {
			t.Fatal("unknown/malformed metadata accepted")
		}
	}
	state.Operations = append(state.Operations, QuestionStoredOperation{Key: "bad-resolve", Digest: digest, Actor: digest, BudgetKey: digest, Status: "resolved", Revision: 2, At: now})
	raw, _ = json.Marshal(state)
	if _, ok := ParseQuestionStoredState(string(raw), 2, "resolved", now, now.Add(time.Hour)); ok {
		t.Fatal("pending-to-resolved accepted")
	}
}
