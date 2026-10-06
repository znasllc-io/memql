package planner

import (
	"context"
	"strings"
	"testing"
)

func TestClassificationKeepsRecentContextWithoutMutatingTheCapturedHistory(t *testing.T) {
	messages := []map[string]any{}
	for range 100 {
		messages = append(messages, map[string]any{"role": "user", "content": strings.Repeat("Older data ", 2000)})
	}
	messages = append(messages, map[string]any{"role": "user", "content": "Use the PDF format we discussed."})
	input := map[string]any{"id": "chat", "messages": messages, "pageContext": strings.Repeat("Page ", 5000)}
	preview, err := conversationPreview(input)
	if err != nil || len(preview) > 22000 || !strings.Contains(preview, "Use the PDF format") || !strings.Contains(preview, "contextNotice") {
		t.Fatalf("bad classifier preview: length=%d %v", len(preview), err)
	}
	if len(input["messages"].([]map[string]any)) != 101 || messages[0]["content"] != strings.Repeat("Older data ", 2000) {
		t.Fatal("preview erased persisted evidence")
	}
}
func TestConversationalWorkloadPreservesFastPathAndResearchBeforeFile(t *testing.T) {
	for _, tier := range []string{"quick", "lookup", "research", "project"} {
		t.Run(tier, func(t *testing.T) {
			eng := &countingCompileEngine{triage: map[string]any{"complexity": "trivial", "intent": "reply", "requiresFile": false, "workload": tier, "workTitle": "Recall name", "acknowledgement": "I’ll check the name you shared."}}
			req := compileReq()
			req.Input = map[string]any{"conversation": map[string]any{"messages": []any{}}}
			out, err := (&PlannerAgentLoop{engine: eng}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
			if err != nil || out.Reply != (tier == "quick") || out.Workload != tier || len(eng.aiCalls) != 1 {
				t.Fatalf("classifier changed fast path or added inference: %+v %v", out, err)
			}
		})
	}
	yes := true
	decision := sectionableDecision{RequiresFile: &yes, FileName: "birds", FileFormat: "pdf", Workload: "research", RequiresResearch: true}
	bundle, err := synthesizeWorkReasoningBundle(compileReq(), "assistant", decision)
	if err != nil {
		t.Fatal(err)
	}
	source := bundle.Constructs[0].Source
	if !decision.needsReasoningAgent("birds") || !strings.Contains(source, "research := builtin runAgentTurn") || strings.Index(source, "research :=") > strings.Index(source, "composeMaterialize(") || !strings.Contains(source, "draft: toString(research)") {
		t.Fatalf("file rendered before evidence was gathered:\n%s", source)
	}
}

func TestSelfContainedFilesSkipEvidenceAcquisitionRegardlessOfEstimate(t *testing.T) {
	for _, tier := range []string{"quick", "lookup", "research", "project"} {
		d := parseSectionableDecision(map[string]any{"requiresFile": true, "requiresResearch": false, "fileName": "supplied-note", "fileFormat": "pdf", "workload": tier})
		bundle, err := synthesizeWorkReasoningBundle(compileReq(), "assistant", d)
		if err != nil {
			t.Fatal(err)
		}
		src := bundle.Constructs[0].Source
		if d.needsReasoningAgent("supplied note") || strings.Contains(src, "runAgentTurn") || !strings.Contains(src, "composeMaterialize(") {
			t.Fatalf("%s inserted unnecessary evidence work:\n%s", tier, src)
		}
	}
}
