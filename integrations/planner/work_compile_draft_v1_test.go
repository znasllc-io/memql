package planner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// work_compile_draft_v1_test.go -- the work draft is written in edition 2026,
// the grammar the engine parses (memql#5367).

// workDraftVariants synthesizes the draft for every route shape: one turn or
// sections, a text answer or a native file.
func workDraftVariants(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, sectionable := range []bool{false, true} {
		for _, requiresFile := range []bool{false, true} {
			decision := parseSectionableDecision(map[string]any{
				"sectionable": sectionable, "requiresFile": requiresFile,
				"fileName": "report", "fileFormat": "markdown",
				"sections": []map[string]any{{"label": "one", "instruction": "first"}, {"label": "two", "instruction": "second"}},
			})
			bundle, err := synthesizeWorkReasoningBundle(compileReq(), "agent-1", decision)
			if err != nil {
				t.Fatal(err)
			}
			name := "turn"
			if sectionable {
				name = "sections"
			}
			if requiresFile {
				name += "+file"
			}
			out[name] = bundle.Constructs[0].Source
		}
	}
	return out
}

// TestSectionableBundleCompilesInEdition2026: the sectionable generator's
// logic closure -- `return now` -- and its automations compile through the
// real Gate 1.
func TestSectionableBundleCompilesInEdition2026(t *testing.T) {
	bundle, ok := synthesizeSectionableBundle("vendors", "profile two vendors", sectionableDecision{
		Sectionable: true,
		Sections:    []sectionSpec{{Label: "vendor Acme", Instruction: "Profile Acme."}, {Label: "vendor Globex", Instruction: "Profile Globex."}},
	})
	if !ok {
		t.Fatal("synthesize failed")
	}
	bundle = withSectionableLogic(bundle)
	report := memql.SandboxCompileBundle(bundle.Constructs)
	requireAutomationsActuallyCompiled(t, report)
	if !report.OK {
		t.Fatalf("the sectionable bundle does not compile in edition 2026: %s", firstFailureHeadline(report))
	}
}

// draftCalls records every construct call a draft makes, with its arguments
// resolved, and answers each with a text naming the call.
type draftCalls struct {
	args map[string]map[string]any
}

func (d *draftCalls) Execute(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	args, err := sc.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	if err != nil {
		return nil, err
	}
	d.args[step.ID] = args
	now := time.Now()
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: "text of " + step.ID, StartedAt: now, CompletedAt: now}, nil
}

// TestWorkDraftFollowsEdition2026: every draft shape passes Gate 1 and loads
// as the engine loads a validated headline -- and, run with the goal's
// variables as its trigger payload (what adopt.go does), the prompt each call
// passes carries the goal input, bound through the draft's args block, as the
// JSON its heading promises, and the assembly carries every section's value.
func TestWorkDraftFollowsEdition2026(t *testing.T) {
	for name, src := range workDraftVariants(t) {
		t.Run(name, func(t *testing.T) {
			if strings.Contains(src, "concat(") || strings.Contains(src, "field(") {
				t.Fatalf("the edition-2026 draft carries a retired call:\n%s", src)
			}
			construct := memql.SandboxConstruct{Kind: "automation", Name: "workRun_v1_work_run_r1", Source: src}
			report := memql.SandboxCompileBundle([]memql.SandboxConstruct{construct})
			requireAutomationsActuallyCompiled(t, report)
			if !report.OK {
				t.Fatalf("Gate 1 refused the draft: %s\n%s", firstFailureHeadline(report), src)
			}
			auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(src, "work-compile/"+construct.Name+".memql")
			if err != nil {
				t.Fatalf("load: %v\n%s", err, src)
			}
			if len(auto.Steps) == 0 || auto.Steps[0].Exprs == nil {
				t.Fatal("the draft did not load as an edition-2026 automation: its steps carry no parsed expressions")
			}
			calls := &draftCalls{args: map[string]map[string]any{}}
			reg := steps.NewRegistry()
			reg.Register(automations.StepTypeFunction, calls)
			ev := events.Event{Topic: "work.run.dispatched", Payload: map[string]any{"day": "2026-09-04"}}
			exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg}).ExecuteWithEvent(context.Background(), auto, "test", &ev)
			if err != nil || exec.Status != "completed" {
				t.Fatalf("run the draft: %v (%+v)\n%s", err, exec, src)
			}
			const goalInput = "\n\nGoal input (JSON):\n" + `{"day":"2026-09-04"}`
			last := "reason"
			if strings.HasPrefix(name, "sections") {
				last = "assemble"
				// Every section's call carries the goal input, and the
				// assembly carries every section's value by name.
				sections := map[string]any{}
				for id, args := range calls.args {
					if id == last {
						continue
					}
					if prompt, _ := args["prompt"].(string); !strings.HasSuffix(prompt, goalInput) {
						t.Errorf("section %s prompt = %q, want it to end with the goal input", id, prompt)
					}
					sections[id] = "text of " + id
				}
				if len(sections) != 2 {
					t.Fatalf("want two section calls, got %v", calls.args)
				}
				want, _ := json.Marshal(sections)
				field := "prompt"
				if strings.HasSuffix(name, "+file") {
					field = "draft"
				}
				if got, _ := calls.args[last][field].(string); !strings.HasSuffix(got, string(want)) {
					t.Fatalf("assembly %s = %q, want it to end with the sections %s", field, got, want)
				}
				return
			}
			field := "prompt"
			if strings.HasSuffix(name, "+file") {
				field = "statement"
			}
			want := compileReq().Statement + goalInput
			if got := calls.args[last][field]; got != want {
				t.Fatalf("%s = %q, want %q", field, got, want)
			}
		})
	}
}
