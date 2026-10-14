//go:build agent

package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agent"
	"github.com/znasllc-io/memql/integrations/agents"
	work "github.com/znasllc-io/memql/integrations/work"
)

// The provider and one harmless read are synthetic; both replicas use the
// installed feedback tool, work bridge, checkpoint and public decision handler.
type feedbackAnswerProvider struct {
	first    bool
	requests [][]common.ChatMessage
}

func (*feedbackAnswerProvider) Call(context.Context, string) (any, error) {
	return nil, fmt.Errorf("unexpected text call")
}
func (p *feedbackAnswerProvider) CallChatWithTools(_ context.Context, messages []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	p.requests = append(p.requests, append([]common.ChatMessage(nil), messages...))
	if p.first {
		p.first = false
		return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{
			{ID: "completed", Name: "discoverCapabilities", Arguments: `{}`},
			{ID: "scope-call", Name: "requestUserFeedback", Arguments: `{"kind":"text","question":"Which synthetic input should be used?"}`},
			{ID: "unstarted", Name: "discoverCapabilities", Arguments: `{}`},
		}}, nil
	}
	for _, m := range messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[Recorded answer for this ") && strings.Contains(m.Content, feedbackAnswerText) {
			return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{{ID: "done", Name: "respondToUser", Arguments: `{"status":"complete","response":"Correction received without executing an operation"}`}}}, nil
		}
	}
	var tools []string
	for _, message := range messages {
		if message.Role == "tool" {
			tools = append(tools, message.Content)
		}
	}
	return nil, fmt.Errorf("recorded correction missing; synthetic tool results: %v", tools)
}
func (p *feedbackAnswerProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	result, err := p.CallChatWithTools(ctx, messages, tools)
	if err != nil {
		return nil, err
	}
	ch := make(chan common.StreamToolChunk, 1)
	deltas := make([]common.ToolCallDelta, len(result.ToolCalls))
	for n, call := range result.ToolCalls {
		deltas[n] = common.ToolCallDelta{Index: n, ID: call.ID, Name: call.Name, Arguments: call.Arguments}
	}
	ch <- common.StreamToolChunk{ToolCalls: deltas, Done: true}
	close(ch)
	return ch, nil
}

type feedbackAnswerAdapter struct {
	*CognitionEngineAdapter
	effects map[string]int
}

func (a *feedbackAnswerAdapter) ToolDefinitionsForNames([]string) []common.ToolDefinition {
	return a.CognitionEngineAdapter.ToolDefinitionsForNames([]string{"discoverCapabilities", "requestUserFeedback", "respondToUser"})
}
func (a *feedbackAnswerAdapter) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	if name == "discoverCapabilities" {
		a.effects["completed-tool"]++
		return "completed result retained", nil
	}
	return a.CognitionEngineAdapter.ExecuteToolByName(ctx, name, args)
}

const feedbackAnswerText = "Use the already reviewed synthetic input."

func TestWorkDBFeedbackAnswerPreservesExecutionLineage(t *testing.T) {
	// One real replica pair serves all sequential cases; each lane owns its rows.
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gates.Close() })
	effects := map[string]int{}
	var runners []*feedbackAnswerProvider
	var triggers []*workTemplateTrigger
	var turns []*installedBudgetTurn
	for n, e := range []*memql.MemQLEngine{writer, reader} {
		var wi *work.Integration
		for _, plugin := range memql.RegisteredPlugins() {
			if plugin.Name != "work" && plugin.Name != "library" && plugin.Name != "knowledge" {
				continue
			}
			provider, err := plugin.Factory(memql.PluginContext{ResolvePartitionFromContext: func(context.Context) string { return "default" }, ResolveEmbeddingProvider: func(context.Context, string) (memql.EmbeddingAIProvider, error) {
				return nil, fmt.Errorf("synthetic work has no training corpus")
			}, Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow})
			require.NoError(t, err)
			require.NoError(t, e.RegisterIntegration(provider))
			if plugin.Name == "work" {
				wi = provider.(*work.Integration)
			}
		}
		require.NotNil(t, wi)
		runner := &feedbackAnswerProvider{first: n == 0}
		runners = append(runners, runner)
		ai := agents.New(e.Agents(), e)
		ai.SetWorkGoals(&agentWorkGoals{work: wi})
		registry := memql.NewProviderRegistryForTest()
		registry.RegisterWithParamsForTest("fixture", "OpenAI", "fixture", map[string]any{"contextWindow": 200000}, runner)
		rules := memql.NewRuleRegistry()
		require.NoError(t, rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "fixture", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}))
		require.NoError(t, rules.Finalize())
		rtr := router.New(registry, memql.NewPolicyRegistryForTest(map[string][]string{"fixture": {"fixture"}}), rules, nil, e.Logger)
		replier, err := agent.NewReplier(&feedbackAnswerAdapter{CognitionEngineAdapter: &CognitionEngineAdapter{Engine: e}, effects: effects}, rtr, e.Logger)
		require.NoError(t, err)
		turn := &installedBudgetTurn{replierTurnRunner: &replierTurnRunner{replier: replier, engine: e}}
		turns = append(turns, turn)
		ai.SetAgentTurnRunner(turn)
		require.NoError(t, e.RegisterIntegration(ai))
		require.NoError(t, e.RegisterIntegration(&childPauseFixture{engine: e, t: t, effects: effects}))
		require.NoError(t, e.Functions().Upsert(&memql.Function{Name: "fixtureeffect", Type: memql.FunctionTypeBuiltin, Enabled: true, BuiltinArgs: &memql.BuiltinArgContract{Profile: memql.BuiltinArgProfileOptionalObject, Properties: map[string]string{"label": "string"}}, ArgsSchema: &memql.ArgsSchemaConfig{Fields: []*memql.FunctionArgsField{{Name: "label", Type: "string"}}}, FunctionKind: "builtin", Executor: "integration.childPauseFixture.effect"}))
		loader := automations.NewLoader(automations.LoaderOptions{Functions: e.Functions()})
		tpl := &workTemplate{members: map[string]*automations.Automation{}}
		for _, src := range []string{
			`@template automation section { builtin fixtureeffect(label: "child-prefix"); answer := builtin runAgentTurn(prompt: "Complete the synthetic section"); builtin fixtureeffect(label: "child-tail"); return answer }`,
			`@template automation parent { builtin fixtureeffect(label: "completed-section"); answer := automation section(); builtin fixtureeffect(label: "later-section"); return answer }`,
		} {
			a, err := loader.CompileSource(strings.ReplaceAll(src, "; ", "\n"), "feedback-answer.memql")
			require.NoError(t, err)
			tpl.members[a.Name] = a
		}
		triggers = append(triggers, &workTemplateTrigger{app: &App{engine: e, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: e.Logger}, template: tpl})
	}
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%v", background), func(t *testing.T) {
			clear(effects)
			for n, runner := range runners {
				runner.first = n == 0
				runner.requests = nil
				turns[n].background = background
			}
			key := id.NewShortId()
			owner, runID, goalID := "v1:identity:user:"+key+"owner", key+"run", key+"goal"
			person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			ctx := common.ContextWithRun(memql.WithActingAgentRole(person, "assistant"), common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive})
			write := func(name string, args map[string]any) {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
			}
			call := func(name string, args map[string]any) {
				t.Helper()
				src, err := parser.RenderCall(name, args)
				require.NoError(t, err)
				_, err = reader.Execute(person, "builtin "+src)
				require.NoError(t, err)
			}
			query := func(name string, args map[string]any) []map[string]any {
				t.Helper()
				src, err := parser.RenderCall(name, args)
				require.NoError(t, err)
				r, err := reader.Execute(memql.ContextWithFreshRead(person), "query "+src)
				require.NoError(t, err)
				return memql.MaterializeRows(r)
			}
			load := func() *automations.RunJournal {
				t.Helper()
				j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
				require.NoError(t, err)
				return j
			}
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic ordinary feedback", "origin": "user"})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": triggers[0].template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			write("updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 3, "retries": 2}})
			var previousApproval, previousChild string
			for attempt := 0; attempt < 2; attempt++ {
				ex := triggers[0].executor()
				expectedRerun := ""
				var runErr error
				if attempt == 0 {
					_, runErr = ex.ExecuteAdopted(ctx, triggers[0].template.members["parent"], automations.RunAdoption{RunId: runID})
				} else {
					// Model the historical terminal refusal after an answer was saved.
					// Recovery is a PUBLIC new attempt; no old lineage is backfilled.
					write("updateWorkRun", map[string]any{"runId": runID, "status": "failed", "errorCode": "rerun_refused", "rerun": map[string]any{}})
					call("rerunStep", map[string]any{"runId": runID, "stepKey": "answer"})
					j := load()
					require.NotNil(t, j.Rerun)
					expectedRerun = j.Rerun.RequestId
					require.NotEmpty(t, expectedRerun)
					prepared, options, err := automations.PrepareRerun(j, nil, triggers[0].template.members["parent"])
					require.NoError(t, err)
					ctx, err = workExecutionContext(memql.WithActingAgentRole(person, "assistant"), j, nil, nil)
					require.NoError(t, err)
					runners[0].first = true
					_, runErr = ex.ResumeFrom(ctx, prepared, triggers[0].template.members["parent"], options)
					// The original answer survives failed state and reaches the new
					// turn via the owned run-history reader, independent of rerun ID.
					found := false
					for _, m := range runners[0].requests[len(runners[0].requests)-1] {
						found = found || m.Role == "user" && strings.HasPrefix(m.Content, "[Recorded answer for this run;") && strings.Contains(m.Content, feedbackAnswerText)
					}
					require.True(t, found, "recorded answer must survive supported new-attempt recovery")
				}
				ex.Close()
				var wait *workstate.HumanWait
				require.ErrorAs(t, runErr, &wait)
				j := load()
				step := j.FailedStep
				childID := j.StepStates[step].ChildRunId
				require.NotEmpty(t, childID)
				require.NotEqual(t, previousApproval, wait.ApprovalID)
				require.NotEqual(t, previousChild, childID)
				previousApproval, previousChild = wait.ApprovalID, childID
				beforeState := j.StepStates[step]
				call("decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "answered", "answer": map[string]any{"text": feedbackAnswerText}})
				j = load()
				require.Equal(t, "running", j.Status)
				actualRerun := ""
				if j.Rerun != nil {
					actualRerun = j.Rerun.RequestId
				}
				require.Equal(t, expectedRerun, actualRerun, "ordinary answer must not manufacture a new execution request")
				require.Equal(t, beforeState, j.StepStates[step], "answer must not alter the paused generation")
				require.Equal(t, wait.ApprovalID, j.HumanResumeId)
				_, ready := (&workRunDispatcher{app: triggers[1].app}).PrepareDispatch(person, work.DispatchRequest{RunId: runID, HumanResumeId: wait.ApprovalID, RerunRequestId: expectedRerun})
				require.True(t, ready)
				prepared, options, code, err := workRerunResumption(j, nil, triggers[1].template.members["parent"])
				require.NoError(t, err, code)
				if prepared == nil {
					prepared = j
				}
				peer := triggers[1].executor()
				done, err := peer.ResumeFrom(ctx, prepared, triggers[1].template.members["parent"], options)
				peer.Close()
				require.NoError(t, err)
				require.Equal(t, "completed", done.Status)
				require.Len(t, runners[1].requests, attempt+1)
				require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": attempt + 1, "completed-tool": attempt + 1, "child-tail": attempt + 1, "later-section": attempt + 1}, effects)
				require.Contains(t, runners[1].requests[attempt], common.ChatMessage{Role: "tool", ToolCallId: "completed", Name: "discoverCapabilities", Content: "completed result retained"})
				final := query("workRunForOwner", map[string]any{"runId": runID})[0]
				require.Equal(t, float64(3), final["spent"].(map[string]any)["modelCalls"])
				require.Equal(t, float64(2), final["spent"].(map[string]any)["retries"])
				approval := query("workApprovalForOwner", map[string]any{"approvalId": wait.ApprovalID})[0]
				require.Equal(t, "answered", approval["decision"])
				require.Equal(t, feedbackAnswerText, approval["answer"].(map[string]any)["text"])
				child, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, childID)
				require.NoError(t, err)
				require.Equal(t, "succeeded", child.Status)
			}
		})
	}
}
