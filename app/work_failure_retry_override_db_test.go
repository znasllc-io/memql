//go:build agent

package app

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agents"
	work "github.com/znasllc-io/memql/integrations/work"
)

type retryDirectionTurn struct {
	history []*memqlv1.AgentTurnMessage
	run     common.RunContext
}

func (*retryDirectionTurn) ResolveOwnerAgent(context.Context, string) (string, error) {
	return "fixture-agent", nil
}
func (f *retryDirectionTurn) RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	f.run, _ = common.RunFromContext(ctx)
	f.history = msg.History
	return "", fmt.Errorf("fixture stopped after capturing the installed context builder")
}

// The public failure decision is handled by another engine; the actual
// executor and installed runAgentTurn builder then construct the next input.
func TestWorkDBFailureRetryKeepsTheAuthoredInstruction(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	key := id.NewShortId()
	owner, runID, goalID := key+"owner", key+"run", key+"goal"
	person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	var integrations []*work.Integration
	for _, e := range []*memql.MemQLEngine{writer, reader} {
		for _, plugin := range memql.RegisteredPlugins() {
			if plugin.Name != "work" {
				continue
			}
			provider, err := plugin.Factory(memql.PluginContext{Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow})
			require.NoError(t, err)
			wi := provider.(*work.Integration)
			require.NoError(t, e.RegisterIntegration(wi))
			integrations = append(integrations, wi)
		}
	}
	require.Len(t, integrations, 2)
	runner := &retryDirectionTurn{}
	ai := agents.New(reader.Agents(), reader)
	ai.SetAgentTurnRunner(runner)
	require.NoError(t, reader.RegisterIntegration(ai))
	loader := automations.NewLoader(automations.LoaderOptions{Functions: reader.Functions()})
	automation, err := loader.CompileSource("@template automation retryFixture {\nanswer := builtin runAgentTurn(prompt: \"Obsolete original workflow\")\nreturn answer\n}", "retry-direction.memql")
	require.NoError(t, err)
	target := automation.Steps[0].ID
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
	}
	query := func(name string, args map[string]any) []map[string]any {
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		result, err := reader.Execute(memql.ContextWithFreshRead(person), "query "+call)
		require.NoError(t, err)
		return memql.MaterializeRows(result)
	}
	const instruction = "Verify the completed document through an isolated browser. Do not use desktop input."
	const failure = "app session: retained transcript file is missing"
	before := time.Now().UTC().Add(-time.Minute)
	override := map[string]any{"prompt": instruction, "wholePrompt": true, "model": "app:codex:test-model", "effort": "xhigh", "requestedBy": owner}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Original document workflow", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": automation.Name, "templateFingerprint": automation.DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": before.Format(time.RFC3339Nano)})
	write("createWorkStep", map[string]any{"stepId": runID + "-" + target, "runId": runID, "key": target, "seq": 0, "stepType": "function", "kind": "reasoning", "call": map[string]any{"construct": "function", "name": "runAgentTurn"}, "status": "failed", "attempt": 2, "version": 2, "override": override, "startedAt": before.Format(time.RFC3339Nano), "errorMessage": failure})
	write("updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{target}, "head": map[string]any{target: map[string]any{"version": 2}}, "rerun": map[string]any{"requestId": "authored-attempt", "stepKey": target, "reason": "rerun", "override": override, "requestedAt": before.Format(time.RFC3339Nano), "versions": map[string]any{target: 2}}, "spent": map[string]any{"retries": 2}})
	req := workstate.FailureApproval(workstate.ApprovalKindPlanReview, runID, target, workstate.SymptomEnvironment, failure, "Retry after repairing retention?", workstate.Evidence{}, time.Now().UTC(), time.Hour)
	approval, err := integrations[0].RaiseApproval(person, owner, work.ApprovalSeed{RunId: runID, StepKey: target, Kind: req.Kind, Subject: req.Subject, ArtifactHash: req.ArtifactHash, Question: req.Question, Options: req.Options, RequestedAt: req.RequestedAt, ExpiresAt: req.ExpiresAt})
	require.NoError(t, err)
	write("updateWorkRun", map[string]any{"runId": runID, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approval, "approvalKind": req.Kind, "since": time.Now().UTC().Format(time.RFC3339Nano)}})
	call, err := parser.RenderCall("decideApproval", map[string]any{"approvalId": approval, "decision": "answered", "answer": map[string]any{"value": "retry"}})
	require.NoError(t, err)
	_, err = reader.Execute(person, "builtin "+call)
	require.NoError(t, err)
	row := query("workRunForOwner", map[string]any{"runId": runID})[0]
	rerun := row["rerun"].(map[string]any)
	require.NotEqual(t, "authored-attempt", rerun["requestId"])
	require.Equal(t, override, rerun["override"])
	require.Equal(t, before.Format(time.RFC3339Nano), rerun["overrideRequestedAt"])
	require.Equal(t, float64(2), row["spent"].(map[string]any)["retries"])
	journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
	require.NoError(t, err)
	ctx, err := workExecutionContext(person, journal, nil, nil)
	require.NoError(t, err)
	saved, opts, _, err := workRerunResumption(journal, nil, automation)
	require.NoError(t, err)
	trigger := &workTemplateTrigger{app: &App{engine: reader, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: reader.Logger}, template: &workTemplate{members: map[string]*automations.Automation{automation.Name: automation}}}
	executor := trigger.executor()
	defer executor.Close()
	_, err = executor.ResumeFrom(ctx, saved, automation, opts)
	require.ErrorContains(t, err, "fixture stopped after capturing")
	var active []string
	for _, message := range runner.history {
		if message.Name == common.WorkInstructionMessageName {
			active = append(active, message.Content)
		}
	}
	require.Equal(t, []string{instruction}, active)
	require.Equal(t, "app:codex:test-model", runner.run.Override.Model)
	require.Equal(t, "xhigh", runner.run.Override.Effort)
}
