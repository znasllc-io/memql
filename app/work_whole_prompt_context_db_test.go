//go:build agent

package app

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agents"
	work "github.com/znasllc-io/memql/integrations/work"
	"strings"
	"testing"
	"time"
)

// The installed builtin builds context before real nested permission waits.
// Only model choices and harmless fixture effects are controlled.
func TestWorkDBWholePromptSurvivesNestedPermissionResumes(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	key := id.NewShortId()
	owner, runID, goalID := key+"owner", key+"run", key+"goal"
	person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive})
	effects := map[string]int{}
	var runners []*permissionGuidanceTurn
	var triggers []*workTemplateTrigger
	for _, e := range []*memql.MemQLEngine{writer, reader} {
		var wi *work.Integration
		for _, plugin := range memql.RegisteredPlugins() {
			if plugin.Name != "work" {
				continue
			}
			provider, err := plugin.Factory(memql.PluginContext{Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow})
			require.NoError(t, err)
			wi = provider.(*work.Integration)
		}
		require.NotNil(t, wi)
		require.NoError(t, e.RegisterIntegration(wi))
		runner := &permissionGuidanceTurn{engine: e, work: wi, effects: effects}
		runners = append(runners, runner)
		ai := agents.New(e.Agents(), e)
		ai.SetAgentTurnRunner(runner)
		require.NoError(t, e.RegisterIntegration(ai))
		require.NoError(t, e.RegisterIntegration(&childPauseFixture{engine: e, t: t, effects: effects}))
		require.NoError(t, e.Functions().Upsert(&memql.Function{Name: "fixtureeffect", Type: memql.FunctionTypeBuiltin, Enabled: true, BuiltinArgs: &memql.BuiltinArgContract{Profile: memql.BuiltinArgProfileOptionalObject, Properties: map[string]string{"label": "string"}}, ArgsSchema: &memql.ArgsSchemaConfig{Fields: []*memql.FunctionArgsField{{Name: "label", Type: "string"}}}, FunctionKind: "builtin", Executor: "integration.childPauseFixture.effect"}))
		loader := automations.NewLoader(automations.LoaderOptions{Functions: e.Functions()})
		tpl := &workTemplate{members: map[string]*automations.Automation{}}
		for _, src := range []string{
			`@template automation section { builtin fixtureeffect(label: "child-prefix"); answer := builtin runAgentTurn(prompt: "Complete the synthetic section"); builtin fixtureeffect(label: "child-tail"); return answer }`,
			`@template automation parent { builtin fixtureeffect(label: "completed-section"); answer := automation section(); builtin fixtureeffect(label: "later-section"); return answer }`,
		} {
			a, err := loader.CompileSource(strings.ReplaceAll(src, "; ", "\n"), "whole-prompt-context.memql")
			require.NoError(t, err)
			tpl.members[a.Name] = a
		}
		triggers = append(triggers, &workTemplateTrigger{app: &App{engine: e, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: e.Logger}, template: tpl})
	}
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
	}
	query := func(e *memql.MemQLEngine, name string, args map[string]any) []map[string]any {
		t.Helper()
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		r, err := e.Execute(memql.ContextWithFreshRead(person), "query "+call)
		require.NoError(t, err)
		return memql.MaterializeRows(r)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic whole-prompt instruction through nested waits", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": triggers[0].template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 3, "tokensLocal": 700, "retries": 2}})
	decide := func(id, decision string, answer map[string]any) {
		t.Helper()
		call, err := parser.RenderCall("decideApproval", map[string]any{"approvalId": id, "decision": decision, "answer": answer})
		require.NoError(t, err)
		r, err := reader.Execute(person, "builtin "+call)
		require.NoError(t, err)
		require.Equal(t, true, memql.MaterializeRows(r)[0]["runResumed"])
	}
	ex := triggers[0].executor()
	defer ex.Close()
	_, err = ex.ExecuteAdopted(ctx, triggers[0].template.members["parent"], automations.RunAdoption{RunId: runID})
	var first *workstate.HumanWait
	require.ErrorAs(t, err, &first)
	load := func() *automations.RunJournal {
		t.Helper()
		j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
		require.NoError(t, err)
		return j
	}
	journal := load()
	childID := journal.StepStates[journal.FailedStep].ChildRunId
	require.NotEmpty(t, childID)
	checkpoint := query(reader, "workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": query(reader, "workApprovalForOwner", map[string]any{"approvalId": first.ApprovalID})[0]["stepKey"]})
	require.Len(t, checkpoint, 1)
	// First execution stopped on the compiled instruction. A fresh
	// session rerun edits the whole prompt, as persisted by Apps intake.
	decide(first.ApprovalID, "rejected", nil)
	target := journal.FailedStep
	call, err := parser.RenderCall("rerunStep", map[string]any{"runId": runID, "stepKey": target})
	require.NoError(t, err)
	_, err = writer.Execute(person, "builtin "+call)
	require.NoError(t, err)
	row := query(writer, "workRunForOwner", map[string]any{"runId": runID})[0]
	rerun := row["rerun"].(map[string]any)
	const current = "Verify the completed inventory, publish after its checks pass, and retain the evidence."
	rerun["override"] = map[string]any{"prompt": current, "wholePrompt": true, "requestedBy": owner, "model": "app:codex:test-model", "effort": "xhigh"}
	write("updateWorkRun", map[string]any{"runId": runID, "rerun": rerun})
	peerIndex := 1
	resume := func() (*automations.AutomationExecution, error) {
		j := load()
		c, err := workExecutionContext(person, j, nil, nil)
		if err != nil {
			return nil, err
		}
		saved, opts, _, err := workRerunResumption(j, nil, triggers[1].template.members["parent"])
		if err != nil {
			return nil, err
		}
		peer := triggers[peerIndex].executor()
		defer peer.Close()
		return peer.ResumeFrom(c, saved, triggers[1].template.members["parent"], opts)
	}
	_, err = resume()
	var replacement *workstate.HumanWait
	require.ErrorAs(t, err, &replacement)
	first = replacement
	journal = load()
	childID = journal.StepStates[target].ChildRunId
	assertCurrent := func(messages []common.ChatMessage) {
		t.Helper()
		var active []string
		for _, m := range messages {
			if common.IsWorkInstruction(m) {
				active = append(active, m.Content)
			}
		}
		require.Equal(t, []string{current}, active, "the actual native context builder must retain the effective instruction before any checkpoint")
	}
	require.Len(t, runners[1].requests, 1)
	assertCurrent(runners[1].requests[0])
	before := query(reader, "workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": target})
	require.Len(t, before, 1)
	var immutable string
	require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, before[0]["id"]).Scan(person, &immutable))
	peerIndex = 0 // Resume the checkpoint on the other engine, using durable rows.

	decide(first.ApprovalID, "answered", map[string]any{"feedback": permissionCorrection})
	journal = load()
	require.Equal(t, "running", journal.Status)
	require.NotNil(t, journal.Rerun)
	require.Equal(t, childID, journal.StepStates[journal.FailedStep].ChildRunId)
	require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2, "completed-tool": 2}, effects)
	originalRequest := permission.Request{OwnerID: owner, AgentID: "fixture-agent", RunID: runID, Tool: "workerHost", Action: "exec", Args: map[string]any{"cmd": "printf original", "cwd": "/fixture/repository", "timeoutSec": 60}}
	grant, err := runners[1].work.Find(ctx, originalRequest)
	require.NoError(t, err)
	require.Nil(t, grant)
	_, err = runners[1].work.Claim(ctx, first.ApprovalID, originalRequest, "fixture-worker", "fixture-machine")
	require.Error(t, err)
	_, err = resume()
	var corrected *workstate.HumanWait
	require.ErrorAs(t, err, &corrected)
	require.NotEqual(t, first.ApprovalID, corrected.ApprovalID, "a corrected command needs a new exact receipt")
	require.Len(t, runners[0].requests, 2)
	assertCurrent(runners[0].requests[1])
	require.Contains(t, runners[0].requests[1], common.ChatMessage{Role: "tool", ToolCallId: "saved", Name: "effect", Content: "effect already performed"})
	require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2, "completed-tool": 2}, effects)
	old := query(reader, "workApprovalForOwner", map[string]any{"approvalId": first.ApprovalID})[0]
	require.Equal(t, "answered", old["decision"])
	require.Empty(t, old["permissionUsedAt"])
	var unchanged string
	require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, before[0]["id"]).Scan(person, &unchanged))
	require.Equal(t, immutable, unchanged, "restoration must not rewrite its source checkpoint")
	peerIndex = 1
	decide(corrected.ApprovalID, "approved", map[string]any{"permissionMode": "once"})
	done, err := resume()
	require.NoError(t, err)
	require.Equal(t, "completed", done.Status)
	require.Len(t, runners[1].requests, 2)
	assertCurrent(runners[1].requests[1])
	// Later human correction remains exact and does not become permission.
	require.Contains(t, runners[1].requests[1][len(runners[1].requests[1])-1].Content, "Recorded answer")
	require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2, "completed-tool": 2, "corrected-command": 1, "child-tail": 1, "later-section": 1}, effects)
	newReceipt := query(writer, "workApprovalForOwner", map[string]any{"approvalId": corrected.ApprovalID})[0]
	require.NotEmpty(t, newReceipt["permissionUsedAt"])
	finalChild, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), writer, childID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", finalChild.Status)
	finalRun := query(writer, "workRunForOwner", map[string]any{"runId": runID})[0]
	require.Equal(t, float64(3), finalRun["spent"].(map[string]any)["modelCalls"])
	require.Equal(t, float64(2), finalRun["spent"].(map[string]any)["retries"])
}
