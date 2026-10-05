package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work"
)

func conversationReq(statement, previous string) CompileRequest {
	req := compileReq()
	req.Statement = statement
	req.Input = map[string]any{"conversation": map[string]any{"messages": []map[string]any{{"role": "assistant", "content": previous}}}}
	return req
}

func TestConversationFollowupUsesHistoryAndNeverTextOnlyReuse(t *testing.T) {
	req := conversationReq("Jose", "If you'd like me to know your name, feel free to tell me.")
	eng := &countingCompileEngine{triage: map[string]any{"intent": "reply", "complexity": "trivial", "requiresFile": false}, catalogue: []map[string]any{{"id": "wrong", "name": "dailyReminder", "goalSignature": work.GoalSignature("Jose", []string{"conversation"}), "reliability": 1.0}}}
	near := &countingNearMatcher{}
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, near, realSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Reply || out.Route != work.RouteTrivial || len(eng.aiCalls) != 1 || near.calls != 0 {
		t.Fatalf("unexpected routing: %+v, %v, near %d", out, eng.aiCalls, near.calls)
	}
	if !strings.Contains(eng.aiData[0]["conversation"].(string), "your name") {
		t.Fatalf("classifier lost history: %v", eng.aiData)
	}
	if remaining := time.Until(eng.deadlines[0]); remaining <= 0 || remaining > 60*time.Second {
		t.Fatalf("unbounded triage: %v", remaining)
	}
	for _, q := range eng.queries {
		if strings.Contains(q, "ForGoalSignature") {
			t.Fatalf("follow-up reached text-only catalogue: %s", q)
		}
	}
	req.Input = conversationReq("Jose", "Who should receive the report?").Input
	other, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	if other.Signature == out.Signature {
		t.Fatal("different conversational meanings shared a signature")
	}
}

func TestClassificationFailureCannotEnterAuthoring(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response any
		err      error
	}{
		{"empty", "", nil}, {"malformed", "not json", nil},
		{"unknown", map[string]any{"complexity": "unsure"}, nil},
		{"timeout", nil, context.DeadlineExceeded},
		{"provider failure", nil, errors.New("worker disconnected")},
		{"missing intent", map[string]any{"complexity": "complex", "requiresFile": false}, nil},
		{"missing delivery", map[string]any{"complexity": "trivial", "intent": "reply"}, nil},
		{"conflicting intent", map[string]any{"complexity": "trivial", "intent": "reply", "requiresFile": true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &countingCompileEngine{triage: tc.response, aiErr: tc.err}
			_, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), conversationReq("Jose", "What is your name?"), nil, realSandbox{})
			if err == nil {
				t.Fatal("invalid classification succeeded")
			}
			if len(eng.aiCalls) != 1 || eng.aiCalls[0] != "goalComplexityTriage" {
				t.Fatalf("failed classifier escalated: %v", eng.aiCalls)
			}
			for _, q := range eng.queries {
				if strings.HasPrefix(q, "mutation ") {
					t.Fatalf("classification failure wrote a draft: %s", q)
				}
			}
		})
	}
}

func TestComplexConversationTaskDoesNotAuthorizeAutomation(t *testing.T) {
	for _, sections := range []bool{false, true} {
		eng := &countingCompileEngine{triage: map[string]any{"intent": "task", "complexity": "complex", "requiresFile": false, "sectionable": sections, "sections": []map[string]any{{"label": "unsafe", "effects": []string{"external"}}}}}
		out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), conversationReq("yes", "Should I research the vendors?"), nil, realSandbox{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply || out.Route == work.RouteAuthor || len(eng.aiCalls) != 1 {
			t.Fatalf("one-off task entered authoring: %+v %v", out, eng.aiCalls)
		}
	}
}
