package planner

import (
	"context"
	"encoding/json"
	"reflect"
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
					"Reconcile each criterion applicable to this section with recorded evidence",
					"A change receipt alone does not prove the original requirements were met",
					"Carry unmet criteria and later obligations explicitly in the handoff",
					"do not label them done or perform later sections' work",
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
			"original requirements and applicable instructions alongside the change receipts",
			"Assign every requested acceptance criterion to a section",
			"Earlier outputs reach a section only through named inputs",
			"creating required verification and running existing checks are different obligations",
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

// The classifier response is a fixture, not a model-quality assertion. Execute
// the captured Spine and reload its persisted bundle on a fresh executor to
// prove the declared requirements and instructions survive every handoff.
func TestSectionCriteriaTravelWithChangeAndVerificationReceipts(t *testing.T) {
	const requirements = "Request R: revise the handling procedure and its explanation. Verification must include an observed dry run. Publishing remains later work. Instructions P: retain the original evidence and use the approved review channel."
	const changed = "Change C: revised procedure and explanation saved. Unmet criterion: dry-run verification belongs to the verify section; review publication belongs to publish."
	const verified = "Verification V: dry run observed successfully, receipt V1. Unmet criterion: publication belongs to publish."
	const published = "Publication P: procedure and evidence submitted through the approved review channel, receipt P1."
	replies := map[string]string{"read": requirements, "change": changed, "verify": verified, "publish": published}
	for _, inline := range []bool{false, true} {
		name := "live"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			req := CompileRequest{
				GoalId: "v1:work:goal:criteria", RunId: "v1:work:run:criteria", OwnerUserId: "u1",
				Statement: "Read the request, make the authorized change, verify it, and submit the result for review.",
				Input:     map[string]any{"requestRef": "request-R"},
			}
			sections := []map[string]any{
				{"label": "read", "instruction": "Read the request and applicable instructions, preserving criteria and explicit gaps.", "purpose": "Read a change request",
					"inputs": []string{"requestRef"}, "outputs": []string{"requirements", "instructions"}},
				{"label": "change", "instruction": "Make only the authorized change and account for its applicable criteria.", "purpose": "Apply an authorized change",
					"inputs": []string{"requirements", "instructions"}, "outputs": []string{"changeReceipt"}, "effects": []string{"files"}, "postcondition": "the requested procedure and explanation changes are recorded"},
				{"label": "verify", "instruction": "Perform the required verification against the original criteria and retain its evidence.", "purpose": "Verify a change",
					"inputs": []string{"requirements", "instructions", "changeReceipt"}, "outputs": []string{"verificationReceipt"}, "effects": []string{"machine"}, "postcondition": "the required dry run has a recorded outcome"},
				{"label": "publish", "instruction": "Submit the result and criterion coverage for review, preserving unmet items honestly.", "purpose": "Publish a change for review",
					"inputs": []string{"requirements", "instructions", "changeReceipt", "verificationReceipt"}, "outputs": []string{"publicationReceipt"}, "effects": []string{"external"}, "postcondition": "the approved review channel confirms receipt"},
			}
			if inline {
				for _, section := range sections {
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
			engine := newSectionCatalogEngine(map[string]any{"complexity": "moderate", "requiresFile": false, "sectionable": true, "sections": sections, "assembly": "report the recorded outcomes and unmet criteria"})
			out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.LiveSections) != len(sections) {
				t.Fatalf("expected four section boundaries, got %+v", out.LiveSections)
			}
			for _, section := range out.LiveSections {
				if (section.Inline != "") != inline {
					t.Fatalf("fixture did not exercise %s: %+v", name, section)
				}
			}
			turns, _ := runDraftWithReplies(t, engine.countingCompileEngine, workDraftHeadline(req), req.Input, replies)
			for label, want := range map[string]map[string]string{
				"change":  {"requirements": requirements, "instructions": requirements},
				"verify":  {"requirements": requirements, "instructions": requirements, "changeReceipt": changed},
				"publish": {"requirements": requirements, "instructions": requirements, "changeReceipt": changed, "verificationReceipt": verified},
			} {
				prompt := turns.prompts[replies[label]]
				heading := sectionInputsHeading
				if inline {
					heading = earlierResultsHeading
				}
				_, encoded, ok := strings.Cut(prompt, heading)
				if !ok {
					t.Fatalf("%s lost its declared handoff: %s", label, prompt)
				}
				var got map[string]string
				if err := json.Unmarshal([]byte(encoded), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s inputs = %#v, want %#v", label, got, want)
				}
				for _, required := range []string{"Reconcile each criterion applicable to this section with recorded evidence", "Carry unmet criteria and later obligations explicitly in the handoff", "do not label them done or perform later sections' work"} {
					if !strings.Contains(prompt, required) {
						t.Errorf("%s prompt lost %q", label, required)
					}
				}
			}
			// Later receipts must not appear before the section that produces them.
			if strings.Contains(turns.prompts[changed], verified) || strings.Contains(turns.prompts[verified], published) {
				t.Fatal("handoff invented a future section's result")
			}
		})
	}
}
