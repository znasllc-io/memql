package work

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type retainedOperationProbe struct {
	t      *testing.T
	engine *memql.MemQLEngine
	calls  int
}

func (p *retainedOperationProbe) Execute(ctx context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	p.calls++
	require.Equal(p.t, "finish", step.ID, "completed prefix must be served, never executed")
	sources, err := p.engine.ReadWorkReplanSources(ctx)
	require.NoError(p.t, err)
	require.Contains(p.t, sources, `"fileId":"first"`, "new binding must receive failed step's completed effect independently of its new prompt")
	require.Contains(p.t, sources, `"stepKey":"partial"`)
	require.NotContains(p.t, sources, "foreign-private-file")
	// The fixture's provider reuses the first retained file and performs only
	// the missing second effect. This tests receipt transport, not arbitrary
	// model obedience or transactional exactly-once writes.
	require.NoError(p.t, p.engine.RecordDelegatedToolSource(memql.WithActingAgentId(ctx, "fixture-agent"), "saveFile", map[string]any{"name": "second"}, `{"fileId":"second","verified":true}`))
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: memql.NewResultWithOutput("first+second"), StartedAt: time.Now(), CompletedAt: time.Now()}, nil
}
func TestUnfinishedOperationDBReplanResumesWithPartialEffectsAcrossReplica(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	template := installedReplanRun(t)
	runID := a.openRun(t, map[string]any{"automationName": template["automationName"], "templateFingerprint": template["templateFingerprint"]})
	a.writeVersion(t, runID, runID, "gather", 0, 1, nil, "prepared", map[string]any{"result": map[string]any{"status": "success", "value": "prepared", "valueRecorded": true}})
	message := workstate.UnfinishedStepError("Retain the second file; preserve the verified first file.").Error()
	a.writeVersion(t, runID, runID, "partial", 1, 1, nil, "", map[string]any{"status": "failed", "errorMessage": message})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": "waiting", "stepOrder": []string{"gather", "partial"}, "waitingOn": map[string]any{"kind": "replan", "subject": "partial", "reason": "Finish the remaining operation."}, "spent": map[string]any{"retries": 1, "tokens": 123}})
	runtime := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "partial", Mode: common.RunModeLive}
	ctx := common.ContextWithRun(actorCtx(a.owner), runtime)
	require.NoError(t, a.eng.RecordDelegatedToolSource(memql.WithActingAgentId(ctx, "fixture-agent"), "saveFile", map[string]any{"name": "first"}, `{"fileId":"first","verified":true}`))
	foreign := a.openRun(t, nil)
	foreignContext := runtime
	foreignContext.RunId = foreign
	require.NoError(t, a.eng.RecordDelegatedToolSource(memql.WithActingAgentId(common.ContextWithRun(actorCtx(a.owner), foreignContext), "fixture-agent"), "saveFile", nil, `{"fileId":"foreign-private-file"}`))
	rc, err := b.i.LoadReplanContext(context.Background(), b.owner, runID, "partial")
	require.NoError(t, err)
	require.Contains(t, rc.FailedStep, "partialOperationSources")
	require.Equal(t, "prepared", rowString(rowMap(rc.CompletedSteps[0], "result"), "value"))
	revised, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource("@template\nautomation continued { gather := builtin fixtureGather()\n finish := builtin fixtureFinish()\n}\n", "fixture.memql")
	require.NoError(t, err)
	require.NoError(t, b.i.InstallReplan(context.Background(), b.owner, runID, ReplanTemplate{AutomationName: revised.Name, TemplateConstructId: "fixture-compiled", TemplateFingerprint: revised.DefinitionFingerprint(id.NewUntracked()), StepKeys: []string{"gather", "finish"}, ResumeAt: "finish", Outcome: map[string]any{"replannedFrom": "partial"}}))
	journal, err := automations.LoadRunJournal(actorCtx(a.owner), a.eng, runID)
	require.NoError(t, err)
	journal, opts, err := automations.PrepareRerun(journal, nil, revised)
	require.NoError(t, err)
	probe := &retainedOperationProbe{t: t, engine: a.eng}
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: a.eng, StepRegistry: probe})
	t.Cleanup(executor.Close)
	result, err := executor.ResumeFrom(ctx, journal, revised, opts)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, probe.calls)
	sources, err := b.eng.ReadWorkStepSources(actorCtx(a.owner), runID, "")
	require.NoError(t, err)
	rows := sources["recordedSources"].([]map[string]any)
	first, second := 0, 0
	for _, r := range rows {
		value := r["value"].(map[string]any)
		if value["fileId"] == "first" {
			first++
		}
		if value["fileId"] == "second" {
			second++
		}
	}
	require.Equal(t, 1, first)
	require.Equal(t, 1, second)
	_, err = b.eng.ReadWorkStepSources(actorCtx("different-owner"), runID, "")
	require.Error(t, err)
	require.False(t, strings.Contains(result.Error, "work_step_unfinished"))
}
