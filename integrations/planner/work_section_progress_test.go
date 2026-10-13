package planner

import (
	"context"
	"strings"
	"testing"
	"text/template"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// Exercise the installed DSL, serialized Spine and generated automations all
// the way to the actual runAgentTurn arguments. A source-only assertion would
// miss a completion contract dropped from inline or nested section turns.
func TestSectionTurnsCarryTheirOwnCompletionBoundary(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "live"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			req, triage, _ := invoiceGoal()
			sections := triage["sections"].([]map[string]any)
			triage["sections"] = []map[string]any{sections[0], {
				"label": "archive", "instruction": "Write the complete analysis appendix from the summary.",
				"purpose": "Write the analysis appendix", "inputs": []string{"invoiceSummary"},
				"outputs": []string{"appendix"}, "effects": []string{},
			}, sections[1]}
			if inline {
				for _, section := range triage["sections"].([]map[string]any) {
					section["purpose"] = ""
				}
			}
			spine, err := workintegration.CaptureSpine("")
			if err != nil {
				t.Fatal(err)
			}
			req.Spine, err = workflowhost.SnapshotFromMap(spine.Map())
			if err != nil {
				t.Fatal(err)
			}
			engine := newSectionCatalogEngine(triage)
			out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
			if err != nil {
				t.Fatal(err)
			}
			for _, section := range out.LiveSections {
				if (section.Inline != "") != inline {
					t.Fatalf("fixture did not exercise %s sections: %+v", name, out.LiveSections)
				}
			}
			turns, _ := runDraft(t, engine.countingCompileEngine, workDraftHeadline(req), req.Input)
			for _, answer := range []string{"SUMMARY_TEXT", "ARCHIVE_TEXT", "NOTIFY_TEXT"} {
				prompt := turns.prompts[answer]
				t.Logf("%s %s actual turn:\n%s", name, answer, prompt)
				for _, required := range []string{
					"Current section completion:",
					"Once this section's requested output and any stated postcondition are supported",
					"return that output now using the supplied completion contract",
					"Keep requested prose and drafts complete",
					"Later sections are not unfinished work for this section",
					"name the missing fact or applicable instruction it will resolve",
					"Repeating the same successful fetch or the same truncated prefix is not progress",
					"Do not skip applicable instructions or claim an unmet postcondition",
				} {
					if !strings.Contains(prompt, required) {
						t.Errorf("%s turn lost %q", answer, required)
					}
				}
				if !strings.Contains(prompt, "Overall goal (context only): "+req.Statement) {
					t.Fatal("section completion erased the overall constraints")
				}
			}
			for _, answer := range []string{"SUMMARY_TEXT", "ARCHIVE_TEXT"} {
				if !strings.Contains(turns.prompts[answer], "complete content as text") || !strings.Contains(turns.prompts[answer], "do not create or save files") {
					t.Fatalf("%s lost the complete-content drafting boundary", answer)
				}
			}
			if !strings.Contains(turns.prompts["ARCHIVE_TEXT"], "SUMMARY_TEXT") || !strings.Contains(turns.prompts["ARCHIVE_TEXT"], "Write the complete analysis appendix") {
				t.Fatal("dependent draft lost its full-content request or prior evidence")
			}
			if inline && (!strings.Contains(turns.prompts["SUMMARY_TEXT"], "independent section") || !strings.Contains(turns.prompts["ARCHIVE_TEXT"], "builds on the results of earlier sections")) {
				t.Fatal("independent/dependent drafting prefixes were lost")
			}
			notify := turns.prompts["NOTIFY_TEXT"]
			if !strings.Contains(notify, "SUMMARY_TEXT") || !strings.Contains(notify, "It is finished when: the email to finance was sent") || !strings.Contains(notify, "Make only the changes this section describes") {
				t.Fatal("dependent effectful section lost its prior evidence or postcondition")
			}
			if strings.Contains(turns.prompts["ASSEMBLED"], "Current section completion:") {
				t.Fatal("section-local completion policy leaked into final assembly")
			}
		})
	}
}

// These are prompt-contract checks, not a simulated model-quality result.
// Validate and render the installed declarations with no provider invocation.
func TestSectionProgressPromptsPreserveScopeAndMissingEvidence(t *testing.T) {
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data map[string]any
		want []string
	}{
		{"goalComplexityTriage", map[string]any{"goal": "Read the service request, apply the authorized repair, then verify it."}, []string{
			"observable handoff", "Understood", "a survey of every linked source",
		}},
		{"workContextCheckpoint", map[string]any{
			"task":   "Section: Gather requirements. Return the requirements ledger. Later work: apply and verify the repair.",
			"source": "[0] Request retrieved: repair the loose handle. [1] Applicable safety rule retained; the last instruction page was truncated.",
		}, []string{
			"active section separately from the overall goal", "Later-section work:",
			"Current-section gap:", "missing passage or question",
			"Repeated retrieval does not reset completed evidence",
			"repair the loose handle", "last instruction page was truncated",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt, ok := registry.Get(tc.name)
			if !ok {
				t.Fatalf("missing prompt %s", tc.name)
			}
			if err := prompt.ValidateData(tc.data); err != nil {
				t.Fatal(err)
			}
			rendered, err := prompt.Render(tc.data)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(rendered, want) {
					t.Errorf("rendered prompt lost %q", want)
				}
			}
		})
	}
}
