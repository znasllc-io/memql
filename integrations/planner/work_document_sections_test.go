package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/work"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

func TestDocumentSectionsRefuseMultipleDraftsHiddenInOneCall(t *testing.T) {
	for _, test := range []struct {
		name    string
		outputs []string
		words   int
		effects []string
		valid   bool
	}{
		{"bounded", []string{"draftA"}, 650, nil, true},
		{"twelve outputs are one call", []string{"draft1", "draft2", "draft3", "draft4"}, 650, nil, false},
		{"oversized", []string{"draftA"}, 5000, nil, false},
		{"missing estimate", []string{"draftA"}, 0, nil, false},
		{"duplicate delivery", []string{"draftA"}, 650, []string{"files"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := map[string]any{"assembly": "Combine draftA and draftB", "sections": []map[string]any{
				{"label": "First batch", "instruction": "Write the first two profiles", "outputs": test.outputs, "deliver": true, "estimatedWords": test.words, "effects": test.effects},
				{"label": "Second batch", "instruction": "Write the next two profiles", "outputs": []string{"draftB"}, "deliver": true, "estimatedWords": 700},
			}}
			sections, _, err := parseDocumentSections(plan, 800)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
			if test.valid && len(sections) != 2 {
				t.Fatalf("lost independent steps: %+v", sections)
			}
			if test.valid && !strings.Contains(sections[0].Instruction, "Output limit for this step: 650 words total.") {
				t.Fatal("the planning output limit never reaches the execution step")
			}
			encoded, _ := json.Marshal(plan)
			_, _, fencedErr := parseDocumentSections("```json\n"+string(encoded)+"\n```", 800)
			if (fencedErr == nil) != test.valid {
				t.Fatalf("fenced response changed validation: %v", fencedErr)
			}
		})
	}
}

type documentSectionsEngine struct {
	*countingCompileEngine
	answers         []any
	reviews         []any
	structuredCalls []string
}

func TestDocumentSectionsPreserveEvidenceDependencies(t *testing.T) {
	for _, input := range []string{"evidence", "topic", "misspelledEvidence", "draft"} {
		plan := map[string]any{"assembly": "Deliver draft", "sections": []map[string]any{
			{"label": "Research", "instruction": "Gather evidence", "outputs": []string{"evidence"}, "deliver": false, "estimatedWords": 200},
			{"label": "Draft", "instruction": "Use the supplied evidence", "inputs": []string{input}, "outputs": []string{"draft"}, "deliver": true, "estimatedWords": 600},
		}}
		_, _, err := parseDocumentSections(plan, 800, "topic")
		valid := input == "evidence" || input == "topic"
		if (err == nil) != valid {
			t.Fatalf("input %q: expected valid=%v, error=%v", input, valid, err)
		}
	}
}

func (e *documentSectionsEngine) InvokeAI(ctx context.Context, name string, data map[string]any) (any, error) {
	response, err := e.countingCompileEngine.InvokeAI(ctx, name, data)
	if (name == "workDocumentSections" || name == "workDocumentSectionsRepair") && len(e.answers) > 0 {
		response, e.answers = e.answers[0], e.answers[1:]
	}
	if name == "workDocumentCoverage" {
		response = map[string]any{"ok": true, "message": ""}
		if len(e.reviews) > 0 {
			response, e.reviews = e.reviews[0], e.reviews[1:]
		}
	}
	if failure, ok := response.(error); ok {
		return nil, failure
	}
	return response, err
}

func TestDocumentDecompositionRunsOnPinnedSpineAndMetersRepairs(t *testing.T) {
	plan := map[string]any{"assembly": "Combine earlyDraft then lateDraft, preserving their text", "sections": []map[string]any{
		{"label": "Early profiles", "instruction": "Write the first two profiles, at most 600 words", "purpose": "First two profiles", "outputs": []string{"earlyDraft"}, "deliver": true, "estimatedWords": 600, "reuseIntent": "goalSpecific"},
		{"label": "Later profiles", "instruction": "Write the next two profiles, at most 600 words", "purpose": "Next two profiles", "outputs": []string{"lateDraft"}, "deliver": true, "estimatedWords": 600, "reuseIntent": "goalSpecific"},
	}}
	for _, test := range []struct {
		name          string
		answers       []any
		reviews       []any
		budget, calls int
		valid         bool
	}{
		{"valid", []any{plan}, nil, 0, 3, true},
		{"repair invalid plan", []any{map[string]any{}, plan}, nil, 0, 4, true},
		{"repair timed out planning call", []any{context.DeadlineExceeded, plan}, nil, 0, 4, true},
		{"retry failed coverage call", []any{plan, plan}, []any{context.DeadlineExceeded}, 0, 5, true},
		{"refuse two invalid plans", []any{map[string]any{}, map[string]any{}}, nil, 0, 3, false},
		{"budget before refinement", []any{plan}, nil, 1, 1, false},
		{"budget before review", []any{plan}, nil, 2, 2, false},
		{"budget before repair", []any{map[string]any{}, plan}, nil, 2, 2, false},
		{"repair missing coverage", []any{plan, plan}, []any{map[string]any{"ok": false, "message": "The final chronological batch is missing"}}, 0, 5, true},
		{"refuse unresolved coverage", []any{plan, plan}, []any{map[string]any{"ok": false, "message": "Incomplete"}, map[string]any{"ok": false, "message": "Still incomplete"}}, 0, 5, false},
		{"malformed verdict is not approval", []any{plan, plan}, []any{map[string]any{}, map[string]any{}}, 0, 5, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := compileReq()
			req.MaxModelCalls = test.budget
			pinned, err := workintegration.CaptureSpine("")
			if err != nil {
				t.Fatal(err)
			}
			// The receiving planner only has the serialized workflow, as across replicas.
			req.Spine, err = workflowhost.SnapshotFromMap(pinned.Map())
			if err != nil {
				t.Fatal(err)
			}
			engine := &documentSectionsEngine{countingCompileEngine: &countingCompileEngine{triage: map[string]any{
				"complexity": "moderate", "requiresFile": true, "fileName": "profiles", "fileFormat": "markdown", "sectionable": true,
				"sections": []map[string]any{{"label": "Oversized sketch", "instruction": "Write all profiles", "outputs": []string{"allDrafts"}}},
			}}, answers: test.answers, reviews: test.reviews}
			out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
			if (err == nil) != test.valid || out.ModelCalls != test.calls || len(engine.aiCalls) != test.calls {
				t.Fatalf("out=%+v error=%v calls=%v", out, err, engine.aiCalls)
			}
			if test.valid {
				if !strings.Contains(persistedSource(t, engine.countingCompileEngine, out.AutomationName), "sectionKeys: [") {
					t.Fatal("complete drafts were sent through a whole-document model call")
				}
				if out.Route != work.RouteSectionable || len(out.Sections) != 2 {
					t.Fatalf("refinement lost: %+v", out)
				}
				source := persistedSource(t, engine.countingCompileEngine, out.AutomationName)
				if strings.Contains(source, "Oversized sketch") || !strings.Contains(source, "_Early_profiles") || !strings.Contains(source, "_Later_profiles") {
					t.Fatalf("draft persisted routing sketch instead of refined execution steps: %s", source)
				}
			} else {
				for _, query := range engine.queries {
					if strings.HasPrefix(query, "createAuthoring") {
						t.Fatalf("refused plan wrote a draft: %s", query)
					}
				}
			}
			if len(engine.structuredCalls) != test.calls-1 {
				t.Fatalf("planning or review bypassed structured output: calls=%v structured=%v", engine.aiCalls, engine.structuredCalls)
			}
			refinements := 0
			for index, name := range engine.aiCalls {
				if name != "workDocumentSections" && name != "workDocumentSectionsRepair" {
					continue
				}
				refinements++
				if refinements > 1 && engine.aiData[index]["previousError"] == "" {
					t.Fatal("repair lost diagnostic")
				}
			}
		})
	}
}

func TestDocumentPlanningFailurePreservesRunCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := (&spineScope{}).documentPlanningFailure(ctx, context.DeadlineExceeded)
	if result != nil || err != context.Canceled {
		t.Fatalf("canceled run must stop rather than request a repair: result=%v error=%v", result, err)
	}
}

func (e *documentSectionsEngine) InvokeAIStructured(ctx context.Context, name string, data map[string]any, schemaName string, schema json.RawMessage, strict bool) (string, error) {
	if !strict || !json.Valid(schema) || (schemaName != "documentSections" && schemaName != "documentCoverage") {
		return "", fmt.Errorf("missing strict planning schema")
	}
	e.structuredCalls = append(e.structuredCalls, name)
	return structuredTestResponse(e.InvokeAI(ctx, name, data))
}

func TestDocumentDeliveryMustBeExplicitAndCannotContainOnlyEvidence(t *testing.T) {
	for _, selection := range []any{nil, "yes", false} {
		plan := map[string]any{"assembly": "Deliver the draft", "sections": []map[string]any{
			{"label": "Evidence", "instruction": "Research facts", "outputs": []string{"facts"}, "estimatedWords": 100, "deliver": false},
			{"label": "Chapter", "instruction": "Write final text", "inputs": []string{"facts"}, "outputs": []string{"draft"}, "estimatedWords": 500, "deliver": selection},
		}}
		if _, _, err := parseDocumentSections(plan, 800); err == nil {
			t.Fatalf("ambiguous or empty delivery accepted: %v", selection)
		}
	}
}

func TestDocumentDSLSelectsFinalChaptersWithoutResearchNotes(t *testing.T) {
	req := compileReq()
	var err error
	req.Spine, err = workintegration.CaptureSpine("")
	if err != nil {
		t.Fatal(err)
	}
	dec := parseSectionableDecision(map[string]any{"requiresFile": true, "fileName": "report", "fileFormat": "markdown", "sectionable": true, "sections": []map[string]any{
		{"label": "Research", "instruction": "Gather evidence", "outputs": []string{"facts"}, "deliver": false},
		{"label": "Chapter", "instruction": "Write final prose", "inputs": []string{"facts"}, "outputs": []string{"draft"}, "deliver": true},
	}})
	bundle, err := synthesizeWorkReasoningBundle(req, "agent", dec)
	if err != nil {
		t.Fatal(err)
	}
	source := bundle.Constructs[0].Source
	_, delivery, ok := strings.Cut(source, "assemble :=")
	if !ok || !strings.Contains(delivery, "sectionKeys: [") || !strings.Contains(delivery, "_Chapter") || strings.Contains(delivery, "_Research") || strings.Contains(delivery, "draft:") {
		t.Fatalf("delivery selected research notes or another model call: %s", source)
	}
}
