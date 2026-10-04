package planner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

// work_remedy_test.go -- the planner half of the failure path's two remedies
// (memql#5664), DB-free: the engine answers replanGap from a fixture and runs
// the REAL Gate 1, and the work integration's writes are recorded.

// replanEngine is the planner's engine for a re-plan: replanGap answers with
// the draft it holds, every write is recorded, and Gate 1 is the real sandbox.
type replanEngine struct {
	realSandbox
	mu      sync.Mutex
	queries []string
	aiCalls []string
	aiData  []map[string]any
	answer  any
	err     error
}

func (e *replanEngine) Execute(_ context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, q)
	return nil, nil
}

func (e *replanEngine) InvokeAI(_ context.Context, templateId string, data map[string]any) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.aiCalls = append(e.aiCalls, templateId)
	e.aiData = append(e.aiData, data)
	return e.answer, e.err
}

func (e *replanEngine) InvokeAIChatWithFilteredTools(context.Context, string, map[string]any, []string) (string, error) {
	return "", nil
}

func (e *replanEngine) wrote(construct string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, q := range e.queries {
		if strings.HasPrefix(q, construct+"(") {
			out = append(out, q)
		}
	}
	return out
}

// remedyRecorder is the work integration's side of the seam: the replan
// context it reads, and the writes the remedy makes. It also answers the
// write the remedy made before it installed anything, so a remedy that only
// records an outcome is visible as exactly that.
type remedyRecorder struct {
	mu        sync.Mutex
	context   workintegration.ReplanContext
	readErr   error
	installed []workintegration.ReplanTemplate
	repairs   []string
	asked     []string
	outcomes  []map[string]any
	repairErr error
}

func (r *remedyRecorder) LoadReplanContext(context.Context, string, string, string) (workintegration.ReplanContext, error) {
	return r.context, r.readErr
}

func (r *remedyRecorder) InstallReplan(_ context.Context, _, _ string, t workintegration.ReplanTemplate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed = append(r.installed, t)
	return nil
}

func (r *remedyRecorder) RequestRepair(_ context.Context, _, _, stepKey, violation string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repairs = append(r.repairs, stepKey+": "+violation)
	return r.repairErr
}

func (r *remedyRecorder) AskAboutFailedRemedy(_ context.Context, _, _, _, kind, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, kind+": "+reason)
	return nil
}

// RecordCompileOutcome is the write the remedy used to make: the run back to
// `running` with the draft's length on its outcome, and no template.
func (r *remedyRecorder) RecordCompileOutcome(_ context.Context, _, _ string, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, fields)
	return nil
}

// replanFixture is a run that gathered its tickets and failed drafting the
// summary, with a plan miss on the drafting step.
func replanFixture() workintegration.ReplanContext {
	return workintegration.ReplanContext{
		Statement: "Summarise yesterday's support tickets",
		CompletedSteps: []map[string]any{
			{"key": "gather", "call": map[string]any{"construct": "function", "name": "runAgentTurn"}, "result": map[string]any{"value": "12 tickets"}},
		},
		FailedStep: map[string]any{"key": "draft", "errorMessage": "the summary format the plan assumed does not exist", "symptom": "plan"},
		Recorded:   map[string]string{"gather": "done", "draft": "failed"},
	}
}

// replanSource re-emits the completed `gather` as the prefix and replaces the
// failed `draft` with a step of its own.
const replanSource = `use agents.builtins.{ runAgentTurn }

@template
automation summariseTicketsReplanned {
  gather := builtin runAgentTurn(agentId: "agent-1", prompt: "gather yesterday's tickets")
  write := builtin runAgentTurn(agentId: "agent-1", prompt: "write the summary as a plain list")
}
`

func replanAnswer(t *testing.T, source string, served bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"remainingSteps":      []map[string]any{{"key": "write"}},
		"abandonedAssumption": "a summary template existed",
		"goalAlreadyServed":   served,
		"source":              source,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fenced, as a model answers -- the parser must tolerate it.
	return "```json\n" + string(raw) + "\n```"
}

func newRemedy(eng Engine, rec *remedyRecorder) *WorkRemedy {
	return &WorkRemedy{loop: &PlannerAgentLoop{engine: eng}, reader: rec, writer: rec}
}

// A RE-PLAN INSTALLS ITS DRAFT (memql#5664). It used to make the replanGap
// call, record the draft's LENGTH, and put the run back to `running` on the
// template that had just failed. Now the draft goes the way a compiled draft
// goes -- through the real Gate 1, persisted as a validated bundle bound to
// this run -- and the run is handed the template that came out of it.
func TestReplanInstallsTheDraftThroughTheCompilePath(t *testing.T) {
	eng := &replanEngine{answer: replanAnswer(t, replanSource, false)}
	rec := &remedyRecorder{context: replanFixture()}

	if !newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "the plan is wrong from here on") {
		t.Fatalf("the re-plan did not take the run (asked: %v)", rec.asked)
	}
	if len(eng.aiCalls) != 1 || eng.aiCalls[0] != "replanGap" {
		t.Fatalf("model calls = %v, want exactly one replanGap", eng.aiCalls)
	}
	if got := eng.aiData[0]["completedSteps"]; got == nil {
		t.Fatal("replanGap was not shown the completed prefix; it would re-emit every step that already ran")
	}
	bundles := eng.wrote("createAuthoringBundle")
	if len(bundles) != 1 || !strings.Contains(bundles[0], `sourceRunId: "v1:work:run:r1"`) {
		t.Fatalf("the draft was not persisted as a bundle bound to the run: %v (outcomes recorded instead: %v)", bundles, rec.outcomes)
	}
	if v := eng.wrote("recordBundleValidation"); len(v) != 1 || !strings.Contains(v[0], `status: "validated"`) {
		t.Fatalf("the draft was not recorded as having passed Gate 1: %v", v)
	}
	if len(rec.installed) != 1 {
		t.Fatalf("installed %d templates, want 1 (outcomes recorded instead: %v)", len(rec.installed), rec.outcomes)
	}
	got := rec.installed[0]
	if got.AutomationName != "summariseTicketsReplanned" || got.TemplateConstructId == "" || got.TemplateFingerprint == "" || got.TemplateVersion == "" {
		t.Fatalf("installed %+v: the executing node loads a run's template from exactly these fields", got)
	}
	if got.Outcome["replannedFrom"] != "draft" {
		t.Errorf("outcome = %v, want it to name the step the plan broke at", got.Outcome)
	}
	if len(rec.asked) != 0 {
		t.Errorf("a person was asked (%v) about a re-plan that succeeded", rec.asked)
	}
}

// A DRAFT THAT WOULD RUN A COMPLETED STEP AGAIN IS REFUSED, and the run asks
// a person: nothing is persisted, nothing installed, and the remedy does not
// leave the run on its wait to be re-planned -- at the reasoning level --
// every pass.
func TestReplanRefusesADraftThatWouldRunACompletedStepAgain(t *testing.T) {
	reordered := strings.Replace(replanSource,
		"  gather := builtin runAgentTurn(agentId: \"agent-1\", prompt: \"gather yesterday's tickets\")\n  write := builtin runAgentTurn(agentId: \"agent-1\", prompt: \"write the summary as a plain list\")\n",
		"  write := builtin runAgentTurn(agentId: \"agent-1\", prompt: \"write the summary as a plain list\")\n  gather := builtin runAgentTurn(agentId: \"agent-1\", prompt: \"gather yesterday's tickets\")\n", 1)
	if reordered == replanSource {
		t.Fatal("the fixture did not reorder")
	}
	eng := &replanEngine{answer: replanAnswer(t, reordered, false)}
	rec := &remedyRecorder{context: replanFixture()}

	if !newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "") {
		t.Fatal("the refused draft left the run on its remedy wait, to be re-planned again next pass")
	}
	if len(rec.installed) != 0 || len(eng.wrote("createAuthoringBundle")) != 0 {
		t.Fatalf("a draft that runs `gather` a second time was persisted (%v) or installed (%v)", eng.wrote("createAuthoringBundle"), rec.installed)
	}
	if len(rec.asked) != 1 || !strings.Contains(rec.asked[0], `"gather"`) {
		t.Fatalf("asked %v, want one question naming the completed step", rec.asked)
	}
}

// Every outcome after the model was asked moves the run off its wait; a
// failure that spent nothing leaves it parked to be served again.
func TestReplanSpendsItsAttemptOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    any
		err       error
		wantTook  bool
		wantAsked bool
	}{
		{"the call failed", nil, errors.New("upstream reset the stream"), true, true},
		{"the answer is not a plan", "I could not think of anything", nil, true, true},
		{"the goal is already served", "", nil, true, true},
		{"the draft does not compile", nil, nil, true, true},
		{"no door could serve the call", nil, memql.ErrProviderUnavailable("reasoning"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &replanEngine{answer: tc.answer, err: tc.err}
			switch tc.name {
			case "the goal is already served":
				eng.answer = replanAnswer(t, "", true)
			case "the draft does not compile":
				eng.answer = replanAnswer(t, "automation broken {\n  this is not memql\n}\n", false)
			}
			rec := &remedyRecorder{context: replanFixture()}
			took := newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "")
			if took != tc.wantTook || (len(rec.asked) == 1) != tc.wantAsked {
				t.Fatalf("took=%v asked=%v, want took=%v asked=%v", took, rec.asked, tc.wantTook, tc.wantAsked)
			}
			if len(rec.installed) != 0 {
				t.Fatalf("installed %v", rec.installed)
			}
		})
	}
	// A read that failed spent nothing either: parked, never asked, never
	// re-planned against an empty prefix.
	eng := &replanEngine{answer: replanAnswer(t, replanSource, false)}
	rec := &remedyRecorder{readErr: errors.New("read timeout")}
	if newRemedy(eng, rec).Replan(context.Background(), "v1:work:run:r1", "u1", "draft", "") || len(eng.aiCalls) != 0 || len(rec.asked) != 0 {
		t.Fatalf("a failed read re-planned (calls %v) or asked (%v)", eng.aiCalls, rec.asked)
	}
}

// A REPAIR IS A GUIDED RE-RUN OF THE FAILED STEP. The violation travels as the
// re-run's guidance; a step the run cannot re-run asks a person rather than
// leaving the run on a wait nothing will serve differently.
func TestRepairRequestsAGuidedRerunOfTheFailedStep(t *testing.T) {
	rec := &remedyRecorder{}
	if !newRemedy(&replanEngine{}, rec).Repair(context.Background(), "v1:work:run:r1", "u1", "draft", "the summary must list every ticket") {
		t.Fatal("the repair did not take the run")
	}
	if len(rec.repairs) != 1 || rec.repairs[0] != "draft: the summary must list every ticket" {
		t.Fatalf("repairs = %v, want the failed step re-run with the violation as guidance", rec.repairs)
	}
	if len(rec.outcomes) != 0 {
		t.Fatalf("the repair recorded an outcome and resumed the run (%v) instead of re-running the step with guidance", rec.outcomes)
	}

	refused := &remedyRecorder{repairErr: errors.New("run serves no goal")}
	if !newRemedy(&replanEngine{}, refused).Repair(context.Background(), "v1:work:run:r1", "u1", "draft", "v") || len(refused.asked) != 1 {
		t.Fatalf("a repair that cannot be requested did not ask a person: %v", refused.asked)
	}
	moved := &remedyRecorder{repairErr: workintegration.ErrRemedyNotWaiting}
	if newRemedy(&replanEngine{}, moved).Repair(context.Background(), "v1:work:run:r1", "u1", "draft", "v") || len(moved.asked) != 0 {
		t.Fatalf("a run that moved on was asked about (%v)", moved.asked)
	}
}
