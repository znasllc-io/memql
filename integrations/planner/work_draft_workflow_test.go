package planner

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

func spineWithPhase(t *testing.T, phase, source string) *workflowhost.Snapshot {
	t.Helper()
	base, err := workintegration.CaptureSpine("")
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, d := range base.Constructs {
		sources[d.Kind+":"+d.Name] = d.Source
	}
	sources["automation:"+phase] = source
	ops := map[string]workflowhost.Operation{}
	names := append(append(work.SpineOperations(), work.SpineDraftOperations()...), work.SpineRemedyOperations()...)
	names = append(names, "agentRecoveryContext", "integration.agents.spineRetryHost", "integration.agents.spineObserveComputer", "integration.agents.spineRequestScope")
	for _, name := range names {
		ops[name] = func(context.Context, map[string]any) (any, error) { return nil, fmt.Errorf("admission only") }
	}
	snapshot, err := workflowhost.CaptureEntries(base.Entry, base.Contract, base.Entries, func(kind, name string) (string, error) {
		s, ok := sources[kind+":"+name]
		if !ok {
			return "", fmt.Errorf("missing %s %s", kind, name)
		}
		return s, nil
	}, ops)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDeveloperExecutionRecipeChangesDeliveredInstructionWithoutGoChanges(t *testing.T) {
	snapshot := spineWithPhase(t, workDraftProgram, `@template
 automation workSpineDraftProgram {
 facts := builtin spineDraftFacts()
 return builtin spineDraftAppend(kind: facts.file ? "file" : "answer", instruction: "COMPANY STYLE: " + facts.goal, inputHeading: "\nINPUT: ")
 }`)
	// JSON is all the execution replica receives. No source registry from the
	// installing process or a customer's account setting is involved.
	restored, err := workflowhost.SnapshotFromMap(snapshot.Map())
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []bool{false, true} {
		req := compileReq()
		req.Spine = restored
		dec := parseSectionableDecision(map[string]any{"requiresFile": file, "fileName": "answer", "fileFormat": "markdown"})
		bundle, err := synthesizeWorkReasoningBundle(req, "agent", dec)
		if err != nil || !strings.Contains(bundle.Constructs[0].Source, "COMPANY STYLE") {
			t.Fatalf("custom recipe not used: %+v %v", bundle, err)
		}
	}
}

func TestDraftRecipeCannotSkipOutputContractOrContinueAfterDelivery(t *testing.T) {
	for _, body := range []string{
		`builtin spineDraftAppend(kind: "answer", instruction: "wrong output")`,
		`builtin spineDraftAppend(kind: "file", instruction: "right output")
   builtin spineDraftAppend(kind: "file", instruction: "duplicate output")`,
		`return true`,
	} {
		req := compileReq()
		req.Spine = spineWithPhase(t, workDraftProgram, "@template\nautomation workSpineDraftProgram {\n"+body+"\n}")
		dec := parseSectionableDecision(map[string]any{"requiresFile": true, "fileName": "answer", "fileFormat": "markdown"})
		if _, err := synthesizeWorkReasoningBundle(req, "agent", dec); err == nil {
			t.Fatalf("invalid authored recipe accepted: %s", body)
		}
	}
}

func TestPinnedRemedyRecipeCanChooseHumanReviewWithoutSpendingAModel(t *testing.T) {
	snapshot := spineWithPhase(t, "workSpineReplan", `@template
 automation workSpineReplan { return builtin spineRemedyAsk(reason: "Company policy requires review before changing a plan.") }`)
	eng := &replanEngine{answer: replanAnswer(t, replanSource, false)}
	recorder := &remedyRecorder{context: replanFixture()}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "r1", OwnerUserId: "u1", Spine: snapshot.Map()})
	if !newRemedy(eng, recorder).Replan(ctx, "r1", "u1", "draft", "") || len(eng.aiCalls) != 0 || len(recorder.asked) != 1 || len(recorder.installed) != 0 {
		t.Fatalf("authored review policy not honored: calls=%v asked=%v installed=%v", eng.aiCalls, recorder.asked, recorder.installed)
	}
}

func TestRemedyRecipeCannotSuppressAnUnfinishedSpentAttempt(t *testing.T) {
	snapshot := spineWithPhase(t, "workSpineReplan", `@template
 automation workSpineReplan { builtin spineRemedyGenerate(prompt: "replanGap")
 return true }`)
	eng := &replanEngine{answer: replanAnswer(t, replanSource, false)}
	recorder := &remedyRecorder{context: replanFixture()}
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "r1", OwnerUserId: "u1", Spine: snapshot.Map()})
	if !newRemedy(eng, recorder).Replan(ctx, "r1", "u1", "draft", "") || len(eng.aiCalls) != 1 || len(recorder.asked) != 1 || len(recorder.installed) != 0 {
		t.Fatalf("spent remedy left retryable: calls=%v asked=%v installed=%v", eng.aiCalls, recorder.asked, recorder.installed)
	}
}
