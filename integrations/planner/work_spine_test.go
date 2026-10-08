package planner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

func testSpine(t *testing.T, entry, source string) *workflowhost.Snapshot {
	t.Helper()
	sources := map[string]string{}
	for _, d := range memql.ExtractAutomationSlices(source) {
		sources["automation:"+d.Name] = d.Source
	}
	for _, d := range memql.ExtractFunctionSlices(source) {
		sources["logic:"+d.Name] = d.Source
	}
	ops := map[string]workflowhost.Operation{}
	for _, name := range work.SpineOperations() {
		ops[name] = func(context.Context, map[string]any) (any, error) { t.Fatal("effect at admission"); return nil, nil }
	}
	snapshot, err := workflowhost.Capture(entry, work.SpineContract, func(kind, name string) (string, error) {
		s, ok := sources[kind+":"+name]
		if !ok {
			return "", fmt.Errorf("missing %s:%s", kind, name)
		}
		return s, nil
	}, ops)
	if err != nil {
		t.Fatal(err)
	}
	// Every execution in these tests starts with a deserialized snapshot,
	// as on a receiving planner with no original installed registry.
	snapshot, err = workflowhost.SnapshotFromMap(snapshot.Map())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCompanySpinesChangePolicyWithoutNativeChanges(t *testing.T) {
	source, err := os.ReadFile("../../examples/spine/company/automations.memql")
	if err != nil {
		t.Fatal(err)
	}
	req := compileReq()
	req.Spine = testSpine(t, "companyCatalogSpine", string(source))
	engine := &countingCompileEngine{}
	_, err = (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "reviewed catalog") || len(engine.aiCalls) != 0 {
		t.Fatalf("company miss policy: err=%v calls=%v", err, engine.aiCalls)
	}
	engine.catalogue = []map[string]any{{"id": "c1", "name": "reviewedAutomation", "goalSignature": work.GoalSignature(req.Statement, inputKeys(req.Input))}}
	out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, nil)
	if err != nil || out.AutomationName != "reviewedAutomation" || len(engine.aiCalls) != 0 {
		t.Fatalf("company exact hit: %+v %v", out, err)
	}
	req.Spine = testSpine(t, "companyAnswerSpine", string(source))
	engine = &countingCompileEngine{triage: map[string]any{"complexity": "complex", "requiresFile": false}}
	out, err = (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil || out.Route != work.RouteTrivial || out.AutomationName == "" || len(engine.aiCalls) != 1 {
		t.Fatalf("company answer policy: %+v %v calls=%v", out, err, engine.aiCalls)
	}
}

func TestSpineCannotForgeSuccessOrSuppressNativeRefusal(t *testing.T) {
	for _, body := range []string{
		`return true`,
		`builtin spineUseCandidate(handle: "someone-elses-construct") on error continue
return true`,
		`builtin spineClassify()
builtin spineClassify() on error continue
return true`,
		`builtin spinePersist() on error continue
return true`,
	} {
		req := compileReq()
		req.Spine = testSpine(t, "companyUnsafe", "@template\nautomation companyUnsafe {\n"+body+"\n}")
		engine := &countingCompileEngine{triage: map[string]any{"complexity": "trivial", "requiresFile": false}}
		_, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
		if err == nil || len(engine.aiCalls) > 1 {
			t.Fatalf("guard bypass: %s err=%v calls=%v", body, err, engine.aiCalls)
		}
		for _, q := range engine.queries {
			if strings.Contains(q, "createAuthoring") {
				t.Fatalf("unvalidated write: %s", q)
			}
		}
	}
}

func TestSpineBudgetCheckedBeforeEveryModelStage(t *testing.T) {
	req := compileReq()
	req.MaxModelCalls = 1
	req.Spine = testSpine(t, "budgetSpine", "@template\nautomation budgetSpine { builtin spineClassify()\nbuiltin spineDesign() }")
	engine := &countingCompileEngine{triage: map[string]any{"complexity": "complex", "requiresFile": false}}
	out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err == nil || !strings.Contains(err.Error(), "maxModelCalls") || out.ModelCalls != 1 || len(engine.aiCalls) != 1 {
		t.Fatalf("budget: %+v err=%v calls=%v", out, err, engine.aiCalls)
	}
}

func TestConversationalSpineDropsRefusedSectionsWithoutAuthoring(t *testing.T) {
	req, triage, _ := invoiceGoal()
	req.Input["conversation"] = []any{map[string]any{"role": "user", "content": req.Statement}}
	triage["intent"], triage["requiresFile"], triage["workload"] = "task", false, "research"
	triage["acknowledgement"] = "I will prepare the invoice summary."
	triage["sections"].([]map[string]any)[1]["postcondition"] = ""
	engine := &countingCompileEngine{triage: triage}
	out, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	if err != nil || out.Route != work.RouteTrivial || out.DecompositionRefused == "" || len(out.Sections) != 0 {
		t.Fatalf("whole-goal fallback: %+v %v", out, err)
	}
	if strings.Join(engine.aiCalls, ",") != "goalComplexityTriage" {
		t.Fatalf("conversation authored source after refused sections: %v", engine.aiCalls)
	}
}

type uncertainDraftEngine struct {
	*countingCompileEngine
	writes int
}

func (e *uncertainDraftEngine) Execute(ctx context.Context, q string) (any, error) {
	if strings.HasPrefix(q, "createAuthoringBundle(") {
		e.writes++
		return nil, errors.New("connection lost after write")
	}
	return e.countingCompileEngine.Execute(ctx, q)
}

func TestSpineDoesNotFallbackAfterUncertainPersistence(t *testing.T) {
	req, triage, _ := invoiceGoal()
	engine := &uncertainDraftEngine{countingCompileEngine: &countingCompileEngine{triage: triage}}
	_, err := (&PlannerAgentLoop{engine: engine}).CompileGoalForRun(context.Background(), req, nil, realSandbox{})
	var persistence *draftPersistenceError
	if !errors.As(err, &persistence) || engine.writes != 1 {
		t.Fatalf("uncertain write retried: writes=%d err=%v", engine.writes, err)
	}
}

func TestWorkCompilerRefusesMissingOrCorruptPinnedSpine(t *testing.T) {
	for _, snapshot := range []map[string]any{nil, {"entry": work.DefaultSpine, "contract": work.SpineContract, "version": "wrong"}} {
		req := adapterReq()
		req.Spine = snapshot
		writer := &recordingRunWriter{}
		engine := &countingCompileEngine{}
		NewWorkCompiler(&PlannerAgentLoop{engine: engine}, writer).Compile(context.Background(), req)
		if len(writer.fields) != 1 || writer.fields[0]["status"] != "failed" || len(engine.aiCalls) != 0 {
			t.Fatalf("unpinned work started: writes=%v calls=%v", writer.fields, engine.aiCalls)
		}
	}
}

func TestDefaultSpinePinsTransitivePolicy(t *testing.T) {
	s, err := workintegration.CaptureSpine("")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range s.Constructs {
		names[d.Kind+":"+d.Name] = true
	}
	for _, name := range []string{"automation:defaultWorkSpine", "automation:workSpineDraft", "automation:workSpineAuthor", "logic:workSpineRoute"} {
		if !names[name] {
			t.Fatalf("missing frozen dependency %s", name)
		}
	}
	for _, capability := range workflowhost.ScopedCapabilities(map[string]workflowhost.Operation{"spinePersist": nil, "spineUseCandidate": nil}) {
		if _, err := capability.Handler(context.Background(), nil, 0); err == nil {
			t.Fatal("direct call gained planning authority")
		}
	}
}

func TestSpineMetersEveryEmissionPhase(t *testing.T) {
	req := compileReq()
	req.MaxModelCalls = 1
	engine := &fakeEngine{aiResponder: func(name string, data map[string]any) (any, error) {
		if name != "authoringEmit" {
			return nil, fmt.Errorf("unexpected prompt %s", name)
		}
		auto := data["automationName"].(string)
		return emitJSON(t, []memql.SandboxConstruct{{Kind: "automation", Name: auto, Source: "@template\nautomation " + auto + " { return true }"}}), nil
	}}
	scope, err := (&PlannerAgentLoop{engine: engine}).newSpineScope(req, nil, realSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	// Isolate the emission boundary with a previously validated native design.
	scope.designed = true
	scope.plan = designPlan{AutomationName: "phasedWork", Phases: []resolvedPhase{{Name: "firstPhase"}, {Name: "secondPhase"}}}
	_, err = scope.operations()["spineEmit"](context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "maxModelCalls") || len(engine.aiCalls) != 1 || scope.out.ModelCalls != 1 {
		t.Fatalf("phase calls escaped the run budget: %v calls=%v spent=%d", err, engine.aiCalls, scope.out.ModelCalls)
	}
}

func TestDefaultSpineAuthorsRepairsAndPersistsPassingSource(t *testing.T) {
	initial := memql.SandboxConstruct{Kind: "automation", Name: "dailyDigest", Source: "@template\nautomation dailyDigest { return false }"}
	repaired := initial
	repaired.Source = "@template\nautomation dailyDigest { return true }"
	engine := &fakeEngine{aiResponder: func(name string, _ map[string]any) (any, error) {
		switch name {
		case "goalComplexityTriage":
			return map[string]any{"complexity": "complex", "requiresFile": false}, nil
		case "authoringDesign":
			return designJSON(t, []designDependency{{Kind: "automation", Name: "dailyDigest", Purpose: "Produce the digest", CandidateSource: initial.Source}}), nil
		case "authoringEmit":
			return emitJSON(t, []memql.SandboxConstruct{initial}), nil
		case "authoringRepair":
			return emitJSON(t, []memql.SandboxConstruct{repaired}), nil
		}
		return nil, fmt.Errorf("unexpected prompt %s", name)
	}}
	sandbox := &fakeSandbox{reports: []memql.SandboxReport{failReport("automation", "dailyDigest", "test diagnostic", initial), okReport(repaired)}}
	loop := newDesignLoop(engine)
	out, err := loop.CompileGoalForRun(context.Background(), compileReq(), nil, sandbox)
	if err != nil || out.Route != work.RouteAuthor || out.AutomationName != "dailyDigest" || out.TemplateVersion == "" || out.ModelCalls != 4 {
		t.Fatalf("author workflow: %+v err=%v calls=%v", out, err, engine.aiCalls)
	}
	if got := strings.Join(engine.aiCalls, ","); got != "goalComplexityTriage,authoringDesign,authoringEmit,authoringRepair" {
		t.Fatalf("stage sequence: %s", got)
	}
	if countContains(engine.execCalls, "createAuthoringBundle(") != 1 {
		t.Fatalf("unexpected persistence: %v", engine.execCalls)
	}
}
