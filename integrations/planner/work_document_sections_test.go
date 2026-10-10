package planner

import (
	"context"
	"encoding/json"
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
				{"label": "First batch", "instruction": "Write the first two profiles", "outputs": test.outputs, "estimatedWords": test.words, "effects": test.effects},
				{"label": "Second batch", "instruction": "Write the next two profiles", "outputs": []string{"draftB"}, "estimatedWords": 700},
			}}
			sections, _, err := parseDocumentSections(plan, 800)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
			if test.valid && len(sections) != 2 {
				t.Fatalf("lost independent steps: %+v", sections)
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
	answers []any
}

func (e *documentSectionsEngine) InvokeAI(ctx context.Context, name string, data map[string]any) (any, error) {
	response, err := e.countingCompileEngine.InvokeAI(ctx, name, data)
	if name == "workDocumentSections" && len(e.answers) > 0 {
		response, e.answers = e.answers[0], e.answers[1:]
	}
	return response, err
}

func TestDocumentDecompositionRunsOnPinnedSpineAndMetersRepairs(t *testing.T) {
	plan := map[string]any{"assembly": "Combine earlyDraft then lateDraft, preserving their text", "sections": []map[string]any{
		{"label": "Early profiles", "instruction": "Write the first two profiles, at most 600 words", "purpose": "First two profiles", "outputs": []string{"earlyDraft"}, "estimatedWords": 600, "reuseIntent": "goalSpecific"},
		{"label": "Later profiles", "instruction": "Write the next two profiles, at most 600 words", "purpose": "Next two profiles", "outputs": []string{"lateDraft"}, "estimatedWords": 600, "reuseIntent": "goalSpecific"},
	}}
	for _, test := range []struct {
		name          string
		answers       []any
		budget, calls int
		valid         bool
	}{
		{"valid", []any{plan}, 0, 2, true},
		{"repair invalid plan", []any{map[string]any{}, plan}, 0, 3, true},
		{"refuse two invalid plans", []any{map[string]any{}, map[string]any{}}, 0, 3, false},
		{"budget before refinement", []any{plan}, 1, 1, false},
		{"budget before repair", []any{map[string]any{}, plan}, 2, 2, false},
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
			}}, answers: test.answers}
			out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
			if (err == nil) != test.valid || out.ModelCalls != test.calls || len(engine.aiCalls) != test.calls {
				t.Fatalf("out=%+v error=%v calls=%v", out, err, engine.aiCalls)
			}
			if test.valid {
				if out.Route != work.RouteSectionable || len(out.Sections) != 2 {
					t.Fatalf("refinement lost: %+v", out)
				}
				source := persistedSource(t, engine.countingCompileEngine, out.AutomationName)
				if strings.Contains(source, "Oversized sketch") || !strings.Contains(source, "earlyDraft") || !strings.Contains(source, "lateDraft") {
					t.Fatalf("draft persisted routing sketch instead of refined execution steps: %s", source)
				}
			} else {
				for _, query := range engine.queries {
					if strings.HasPrefix(query, "createAuthoring") {
						t.Fatalf("refused plan wrote a draft: %s", query)
					}
				}
			}
			if len(engine.aiData) > 2 && engine.aiData[2]["previousError"] == "" {
				t.Fatal("repair lost diagnostic")
			}
		})
	}
}
