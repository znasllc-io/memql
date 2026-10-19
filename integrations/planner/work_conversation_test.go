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

func TestCurrentEvidenceRepliesUseToolsWithLookupBudget(t *testing.T) {
	for _, statement := range []string{"Why is it so windy outside?", "¿Por qué hace tanto viento afuera?", "And at my office right now?", "The service outage was caused by a cyberattack, right?", "Is my account balance unchanged today?"} {
		t.Run(statement, func(t *testing.T) {
			eng := &countingCompileEngine{triage: map[string]any{"intent": "reply", "complexity": "trivial", "workload": "quick", "requiresFile": false, "requiresResearch": true, "acknowledgement": "I’ll check the available evidence."}}
			out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), conversationReq(statement, "We were discussing the current conditions."), nil, realSandbox{})
			if err != nil {
				t.Fatal(err)
			}
			if out.Reply || out.Workload != "lookup" {
				t.Fatalf("current evidence entered social reply: %+v", out)
			}
			source := strings.Join(eng.queries, "\n")
			if strings.Contains(source, "workQuickReply") || !strings.Contains(source, "runAgentTurn") {
				t.Fatalf("missing tool-enabled execution: %s", source)
			}
			for _, required := range []string{"First inspect recorded source receipts", "Retrieve only facts still missing", "source observation time", "required identifier from a named record", "different or unverified entity", "status=needs_input", "optional=true"} {
				if !strings.Contains(source, required) {
					t.Fatalf("research requirement %q was lost before execution: %s", required, source)
				}
			}
		})
	}
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
	if remaining := time.Until(eng.deadlines[0]); remaining <= 60*time.Second || remaining > 600*time.Second {
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
		eng := &countingCompileEngine{triage: map[string]any{"intent": "task", "complexity": "complex", "requiresFile": false, "acknowledgement": "I’ll research the vendors we discussed.", "sectionable": sections, "sections": []map[string]any{{"label": "unsafe", "effects": []string{"external"}}}}}
		out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), conversationReq("yes", "Should I research the vendors?"), nil, realSandbox{})
		if err != nil {
			t.Fatal(err)
		}
		if out.Reply || out.Route == work.RouteAuthor || len(eng.aiCalls) != 1 {
			t.Fatalf("one-off task entered authoring: %+v %v", out, eng.aiCalls)
		}
	}
}

func TestVerifiedFollowupCarriesReceiptsThroughClassificationAndQuickReply(t *testing.T) {
	req := conversationReq("Should I wait before retrying?", "The service is degraded.")
	conversation := req.Input["conversation"].(map[string]any)
	conversation["sourceContext"] = map[string]any{"recordedSources": []any{map[string]any{"operation": "readStatus", "checkpointAt": "2026-10-17T04:40:00Z", "value": "status=degraded; observedAt=2026-10-17T04:39:30Z; source=https://status.example.test"}}}
	eng := &countingCompileEngine{triage: map[string]any{"intent": "reply", "complexity": "trivial", "workload": "quick", "requiresFile": false, "requiresResearch": false}}
	out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil || !out.Reply || out.Workload != "quick" {
		t.Fatalf("supported follow-up started research: %+v %v", out, err)
	}
	preview := eng.aiData[0]["conversation"].(string)
	for _, evidence := range []string{"readStatus", "2026-10-17T04:39:30Z", "https://status.example.test"} {
		if !strings.Contains(preview, evidence) {
			t.Fatalf("classifier lost source %q", evidence)
		}
	}
	source := strings.Join(eng.queries, "\n")
	if strings.Contains(source, "runAgentTurn") || strings.Contains(source, "progress: true") || !strings.Contains(source, "workQuickReply") || !strings.Contains(source, "toString(args.conversation)") || !strings.Contains(source, "reason := builtin workReviewReply(draft: draft)") {
		t.Fatalf("quick reply lost persisted source context or started a tool loop: %s", source)
	}
}

func TestCapabilityArtifactWorkloadIncludesGenerationAndKeepsExplicitCeilings(t *testing.T) {
	for _, format := range []string{"png", "wav", "mp4", "xlsx", "glb"} {
		for _, tier := range []string{"quick", "lookup", "research", "project"} {
			t.Run(format+"/"+tier, func(t *testing.T) {
				search := "generate " + format + " artifact"
				eng := &countingCompileEngine{triage: map[string]any{"intent": "task", "complexity": "trivial", "workload": tier, "capabilitySearch": search, "requiresFile": true, "fileFormat": format, "fileName": "output", "acknowledgement": "I’ll create and save the requested artifact."}}
				req := conversationReq("Create the requested artifact", "")
				out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
				if err != nil {
					t.Fatal(err)
				}
				wantWorkload := "research"
				if tier == "project" {
					wantWorkload = tier
				}
				if out.Workload != wantWorkload {
					t.Fatalf("generation was treated as a short lookup: %s", out.Workload)
				}
				if out.CapabilitySearch != search || len(eng.aiCalls) != 1 {
					t.Fatalf("artifact compilation lost discovery intent or added inference: search=%q calls=%v", out.CapabilitySearch, eng.aiCalls)
				}
				limit := work.EffectiveWorkloadCeilings(work.Ceilings{}, out.Workload)
				if tier != "project" && limit.WallClockMs != 45*60*1000 {
					t.Fatalf("unexpected sustained-work allowance: %+v", limit)
				}
				explicit := work.Ceilings{WallClockMs: 60000, MaxModelCalls: 2, CostCeiling: 0.25}
				if got := work.EffectiveWorkloadCeilings(explicit, out.Workload); got.WallClockMs != explicit.WallClockMs || got.MaxModelCalls != explicit.MaxModelCalls || got.CostCeiling != explicit.CostCeiling {
					t.Fatalf("explicit ceiling relaxed: %+v", got)
				}
			})
		}
	}
}
