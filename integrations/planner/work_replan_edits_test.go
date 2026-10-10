package planner

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func TestReplanEditsApplySimultaneouslyAndPreserveUntouchedBytes(t *testing.T) {
	const source = "// résumé\r\nfirst α\nsecond β\nlast\n"
	got, err := applyReplanEdits(source, []replanSourceEdit{
		{Find: "second β", Replace: "third γ"},
		{Find: "first α", Replace: "second β"},
	})
	if err != nil || got != "// résumé\r\nsecond β\nthird γ\nlast\n" {
		t.Fatalf("edits changed untouched bytes or targeted generated text: %q, %v", got, err)
	}
}

func TestReplanEditsRefuseAmbiguousOrInvalidTargets(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		edits        []replanSourceEdit
	}{
		{"empty", "abc", nil},
		{"empty find", "abc", []replanSourceEdit{{Find: "", Replace: "x"}}},
		{"missing", "abc", []replanSourceEdit{{Find: "xyz", Replace: "x"}}},
		{"repeated", "abc abc", []replanSourceEdit{{Find: "abc", Replace: "x"}}},
		{"overlapping occurrences", "aaa", []replanSourceEdit{{Find: "aa", Replace: "x"}}},
		{"overlapping edits", "abcd", []replanSourceEdit{{Find: "abc", Replace: "x"}, {Find: "cd", Replace: "y"}}},
		{"unchanged", "abc", []replanSourceEdit{{Find: "abc", Replace: "abc"}}},
		{"too many", "abc", make([]replanSourceEdit, 17)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := applyReplanEdits(tc.source, tc.edits); err == nil {
				t.Fatal("invalid source edit was accepted")
			}
		})
	}
}

func TestReplanEditsCannotRebindCompletedEvidenceOrEscapeGateOne(t *testing.T) {
	const original = `use agents.builtins.{ runAgentTurn }
@template
automation summariseTickets {
  gather := builtin runAgentTurn(agentId: "agent-1", prompt: "gather yesterday's tickets")
  draft := builtin runAgentTurn(agentId: "agent-1", prompt: "draft all tickets")
}
`
	for _, tc := range []struct {
		name, status string
		edit         replanSourceEdit
	}{
		{"changed completed arguments", "done", replanSourceEdit{"gather yesterday's tickets", "gather different tickets"}},
		{"changed skipped arguments", "skipped", replanSourceEdit{"gather yesterday's tickets", "gather different tickets"}},
		{"new step before prefix", "done", replanSourceEdit{"  gather :=", "  earlier := 42\n  gather :="}},
		{"invalid syntax", "done", replanSourceEdit{"draft :=", "draft ==="}},
		{"unknown annotation", "done", replanSourceEdit{"@template", "@template\n@bogus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := replanFixture()
			rc.TemplateName = "summariseTickets"
			rc.Template = []memql.SandboxConstruct{{Kind: "automation", Name: rc.TemplateName, Source: original}}
			rc.Recorded["gather"] = tc.status
			eng := &replanEngine{answer: map[string]any{"edits": []replanSourceEdit{tc.edit}, "goalAlreadyServed": false}}
			rec := &remedyRecorder{context: rc}
			if !newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "divide unfinished work") {
				t.Fatal("a refused patch left the run eligible for another automatic attempt")
			}
			if len(rec.installed) != 0 || len(rec.asked) != 1 || len(eng.wrote("createAuthoringBundle")) != 0 {
				t.Fatalf("unsafe patch was persisted or installed: installed=%v asked=%v", rec.installed, rec.asked)
			}
			if strings.Contains(tc.name, "arguments") && !strings.Contains(rec.asked[0], "definition") {
				t.Fatalf("wrong reason for prefix refusal: %v", rec.asked)
			}
		})
	}
}

func TestReplanRefusesWholeSourceWhenSealedProgramIsAvailable(t *testing.T) {
	rc := replanFixture()
	rc.TemplateName = "summariseTicketsReplanned"
	rc.Template = []memql.SandboxConstruct{{Kind: "automation", Name: rc.TemplateName, Source: replanSource}}
	eng := &replanEngine{answer: replanAnswer(t, replanSource, false)}
	rec := &remedyRecorder{context: rc}
	newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "divide unfinished work")
	if len(rec.installed) != 0 || len(rec.asked) != 1 || !strings.Contains(rec.asked[0], "requires edits") {
		t.Fatalf("complete source bypassed the edit-only contract: %+v", rec)
	}
}

func TestReplanCannotTurnResearchIntoTextOnlyInference(t *testing.T) {
	const original = `@template
automation summariseTickets {
  gather := logic savedEvidence()
  draft := builtin runAgentTurn(templateId: "lookupEvidence", data: {question: "Verify the claim"})
  deliver := draft
}`
	rc := replanFixture()
	rc.TemplateName = "summariseTickets"
	rc.Template = []memql.SandboxConstruct{{Kind: "automation", Name: rc.TemplateName, Source: original}}
	eng := &replanEngine{answer: map[string]any{"edits": []replanSourceEdit{{Find: "draft := builtin runAgentTurn", Replace: "draft := builtin ai"}}, "goalAlreadyServed": false}}
	rec := &remedyRecorder{context: rc}
	newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "make research smaller")
	if len(rec.installed) != 0 || len(rec.asked) != 1 || !strings.Contains(rec.asked[0], "required execution capability") || len(eng.wrote("createAuthoringBundle")) != 0 {
		t.Fatalf("text-only research substitution escaped the declared Spine policy: installed=%v asked=%v", rec.installed, rec.asked)
	}
}

func TestReplanEditsSplitFailedStepAndPreserveItsDownstreamBinding(t *testing.T) {
	const original = `@template
automation summariseTickets {
  gather := logic savedEvidence()
  draft := logic savedEvidence()
  deliver := draft
}`
	rc := replanFixture()
	rc.TemplateName = "summariseTickets"
	rc.Template = []memql.SandboxConstruct{
		{Kind: "automation", Name: rc.TemplateName, Source: original},
		{Kind: "logic", Name: "savedEvidence", Source: `logic savedEvidence { return {reply: "evidence"} }`},
	}
	eng := &replanEngine{answer: map[string]any{"edits": []replanSourceEdit{{
		Find: "draft := logic savedEvidence()",
		Replace: `first := logic savedEvidence()
  second := logic savedEvidence()
  draft := {reply: first.reply + second.reply}`,
	}}, "goalAlreadyServed": false}}
	rec := &remedyRecorder{context: rc}
	newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "divide unfinished work")
	if len(rec.installed) != 1 || len(rec.asked) != 0 || rec.installed[0].ResumeAt != "first" {
		t.Fatalf("split failed: installed=%v asked=%v", rec.installed, rec.asked)
	}
	if got := strings.Join(rec.installed[0].StepKeys, ","); got != "gather,first,second,draft,deliver" {
		t.Fatalf("split lost the prefix or downstream consumer: %s", got)
	}
}
