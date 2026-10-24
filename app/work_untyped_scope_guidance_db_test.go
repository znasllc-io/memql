//go:build agent

package app

import (
	"context"
	"database/sql"
	"encoding/json"
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
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agent"
	"github.com/znasllc-io/memql/integrations/agent/worker"
	"github.com/znasllc-io/memql/integrations/agents"
	work "github.com/znasllc-io/memql/integrations/work"
)

// Only provider responses and one harmless read are controlled. The Replier
// writes the real pending-call checkpoint and invokes the installed scope tool.
type untypedScopeProvider struct {
	first    bool
	requests [][]common.ChatMessage
	reviews  [][]common.ChatMessage
}

func (*untypedScopeProvider) Call(context.Context, string) (any, error) {
	return nil, fmt.Errorf("unexpected text call")
}

// Keep the installed independent reply review on the resumed replica. Only
// the provider's bounded verdict is synthetic; the saved evidence stays real.
func (p *untypedScopeProvider) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if schema.Name != "work_reply_review" || !schema.Strict {
		return "", fmt.Errorf("unexpected structured call: %s (strict=%v)", schema.Name, schema.Strict)
	}
	p.reviews = append(p.reviews, append([]common.ChatMessage(nil), messages...))
	return `{"missingEvidence":"","reply":"Correction received without executing an operation"}`, nil
}

func (p *untypedScopeProvider) CallChatWithTools(_ context.Context, messages []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	p.requests = append(p.requests, append([]common.ChatMessage(nil), messages...))
	if p.first {
		p.first = false
		return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{
			{ID: "completed", Name: "discoverCapabilities", Arguments: `{}`},
			{ID: "scope-call", Name: "requestComputerUseScope", Arguments: `{"intent":"inspect","requestedScope":"full","sessionKind":"desktop","summary":"Review the synthetic scope","requireLabels":{"fixture":"true"}}`},
			{ID: "unstarted", Name: "discoverCapabilities", Arguments: `{}`},
		}}, nil
	}
	for _, m := range messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "[Recorded answer for this ") && strings.Contains(m.Content, permissionCorrection) {
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
func (p *untypedScopeProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
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

type untypedScopeAdapter struct {
	*CognitionEngineAdapter
	effects map[string]int
}

func (a *untypedScopeAdapter) ToolDefinitionsForNames([]string) []common.ToolDefinition {
	return a.CognitionEngineAdapter.ToolDefinitionsForNames([]string{"discoverCapabilities", "requestComputerUseScope", "respondToUser"})
}
func (a *untypedScopeAdapter) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	if name == "discoverCapabilities" {
		a.effects["completed-tool"]++
		return "completed result retained", nil
	}
	return a.CognitionEngineAdapter.ExecuteToolByName(ctx, name, args)
}

func TestWorkDBUntypedScopeFeedbackContinuesExactPendingCall(t *testing.T) {
	// One real replica pair; negative controls change only this test's synthetic
	// rows and restore their exact payload before trying the next fresh read.
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = gates.Close() })
	effects := map[string]int{}
	var runners []*untypedScopeProvider
	var triggers []*workTemplateTrigger
	var works []*work.Integration
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
		works = append(works, wi)
		runner := &untypedScopeProvider{first: n == 0}
		runners = append(runners, runner)
		workerIntegration := worker.NewIntegration(nil, nil, e, e.Logger)
		workerIntegration.ScopeRequester = wi.AskComputerScope
		require.NoError(t, e.RegisterIntegration(workerIntegration))
		ai := agents.New(e.Agents(), e)
		registry := memql.NewProviderRegistryForTest()
		registry.RegisterWithParamsForTest("fixture", "OpenAI", "fixture", map[string]any{"contextWindow": 200000}, runner)
		rules := memql.NewRuleRegistry()
		require.NoError(t, rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "fixture", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}))
		require.NoError(t, rules.Finalize())
		rtr := router.New(registry, memql.NewPolicyRegistryForTest(map[string][]string{"fixture": {"fixture"}}), rules, nil, e.Logger)
		e.SetAIResolver(rtr)
		replier, err := agent.NewReplier(&untypedScopeAdapter{CognitionEngineAdapter: &CognitionEngineAdapter{Engine: e}, effects: effects}, rtr, e.Logger)
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
			a, err := loader.CompileSource(strings.ReplaceAll(src, "; ", "\n"), "untyped-scope.memql")
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
				runner.reviews = nil
				turns[n].background = background
			}

			key := id.NewShortId()
			owner, runID, goalID := "v1:identity:user:"+key+"owner", key+"run", key+"goal"
			person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			ctx := common.ContextWithRun(memql.WithActingAgentRole(person, "assistant"), common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive})

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
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic scope feedback", "origin": "user"})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": triggers[0].template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
			write("updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 3, "retries": 2}})
			ex := triggers[0].executor()
			defer ex.Close()
			_, err = ex.ExecuteAdopted(ctx, triggers[0].template.members["parent"], automations.RunAdoption{RunId: runID})
			var wait *workstate.HumanWait
			require.ErrorAs(t, err, &wait)
			load := func() *automations.RunJournal {
				t.Helper()
				j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
				require.NoError(t, err)
				return j
			}
			journal := load()
			completedTools := 1
			expectedRerun := ""
			if background {
				// The same public rerun route as an already-retried work section:
				// reject its old question, then start only the selected new version.
				oldApproval := wait.ApprovalID
				oldChild := journal.StepStates[journal.FailedStep].ChildRunId
				for _, act := range []struct {
					name string
					args map[string]any
				}{
					{"decideApproval", map[string]any{"approvalId": oldApproval, "decision": "rejected"}},
					{"rerunStep", map[string]any{"runId": runID, "stepKey": journal.FailedStep}},
				} {
					call, callErr := parser.RenderCall(act.name, act.args)
					require.NoError(t, callErr)
					_, callErr = reader.Execute(person, "builtin "+call)
					require.NoError(t, callErr)
				}
				journal = load()
				require.NotNil(t, journal.Rerun)
				expectedRerun = journal.Rerun.RequestId
				require.NotEmpty(t, expectedRerun)
				prepared, options, prepErr := automations.PrepareRerun(journal, nil, triggers[0].template.members["parent"])
				require.NoError(t, prepErr)
				ctx, err = workExecutionContext(memql.WithActingAgentRole(person, "assistant"), journal, nil, nil)
				require.NoError(t, err)
				runners[0].first = true
				_, err = ex.ResumeFrom(ctx, prepared, triggers[0].template.members["parent"], options)
				require.ErrorAs(t, err, &wait)
				require.NotEqual(t, oldApproval, wait.ApprovalID)
				journal = load()
				require.NotEqual(t, oldChild, journal.StepStates[journal.FailedStep].ChildRunId)
				completedTools = 2
			}

			childID, step := journal.StepStates[journal.FailedStep].ChildRunId, journal.FailedStep
			require.NotEmpty(t, childID)
			call, err := parser.RenderCall("decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "answered", "answer": map[string]any{"feedback": permissionCorrection}})
			require.NoError(t, err)
			_, err = reader.Execute(person, "builtin "+call)
			require.NoError(t, err)
			journal = load()
			require.Equal(t, "running", journal.Status, "feedback must continue the saved turn rather than schedule structural replan")
			approval := query(reader, "workApprovalForOwner", map[string]any{"approvalId": wait.ApprovalID})[0]
			subject := approval["subject"].(map[string]any)
			require.NotContains(t, subject, "computerPermission")
			require.NotContains(t, subject, "scope")
			require.Equal(t, "desktop", subject["sessionKind"], "the feedback question uses an explicit session request, never an omitted operation")
			require.Equal(t, "full", subject["sessionScope"])
			require.Contains(t, subject, "nativeScopeCheckpoint")
			witness := subject["nativeScopeCheckpoint"].(map[string]any)
			require.Equal(t, "scope-call", witness["toolCallId"])
			checkpoint := query(reader, "workContinuationForOwnerRun", map[string]any{"runId": runID, "stepKey": step})[0]
			require.Equal(t, memql.BareShortId(checkpoint["id"].(string)), memql.BareShortId(witness["continuationId"].(string)))
			if expectedRerun == "" {
				require.Nil(t, journal.Rerun)
			} else {
				require.Equal(t, expectedRerun, journal.Rerun.RequestId)
			}
			require.Equal(t, wait.ApprovalID, journal.HumanResumeId)
			require.Equal(t, childID, journal.StepStates[step].ChildRunId)
			_, ready := (&workRunDispatcher{app: triggers[1].app}).PrepareDispatch(person, work.DispatchRequest{RunId: runID, HumanResumeId: wait.ApprovalID, RerunRequestId: expectedRerun})
			require.True(t, ready)
			// No typed or legacy grant can be consumed from an answered correction.
			grant, err := works[1].Find(ctx, permission.Request{OwnerID: owner, AgentID: subject["agentId"].(string), RunID: runID, Tool: "workerHost", Action: "exec", Args: map[string]any{"cmd": "pwd", "cwd": "/fixture"}})
			require.NoError(t, err)
			require.Nil(t, grant)
			sessionRequest := worker.Request{RunId: runID, OwnerUserId: owner, AgentId: subject["agentId"].(string), Tool: "workerComputer", Action: "screenshot", RequireLabels: worker.LabelsFromArgs(subject["requireLabels"])}
			sessionScope, err := (&worker.EngineStore{Engine: reader}).WorkComputerScope(ctx, sessionRequest, time.Now())
			require.NoError(t, err)
			require.Empty(t, sessionScope)
			// Positive reader control with the very same subject and selector:
			// only its decision differs. Restore the actual answered receipt
			// immediately; this is synthetic test data, never a public grant.
			approvalRowID := "v1:work:approval:" + memql.BareShortId(wait.ApprovalID)
			var approvalRaw string
			require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, approvalRowID).Scan(person, &approvalRaw))
			var savedApproval, approvedControl map[string]any
			require.NoError(t, json.Unmarshal([]byte(approvalRaw), &savedApproval))
			require.NoError(t, json.Unmarshal([]byte(approvalRaw), &approvedControl))
			approvedControl["decision"] = "approved"
			insertWorkRow(t, db, approvalRowID, "v1:work:approval", time.Now().UTC(), approvedControl)
			controlScope, err := (&worker.EngineStore{Engine: reader}).WorkComputerScope(ctx, sessionRequest, time.Now())
			require.NoError(t, err)
			require.Equal(t, "full", controlScope)
			execRequest := sessionRequest
			execRequest.Tool, execRequest.Action = "workerHost", "exec"
			controlScope, err = (&worker.EngineStore{Engine: reader}).WorkComputerScope(ctx, execRequest, time.Now())
			require.NoError(t, err)
			require.Empty(t, controlScope, "approving a desktop session must not permit commands")
			insertWorkRow(t, db, approvalRowID, "v1:work:approval", time.Now().UTC(), savedApproval)
			sessionScope, err = (&worker.EngineStore{Engine: reader}).WorkComputerScope(ctx, sessionRequest, time.Now())
			require.NoError(t, err)
			require.Empty(t, sessionScope)

			prepare := func(j *automations.RunJournal) (*automations.RunJournal, *automations.ResumeOptions) {
				t.Helper()
				if j.Rerun == nil {
					return j, nil
				}
				prepared, options, code, prepErr := workRerunResumption(j, nil, triggers[1].template.members["parent"])
				require.NoError(t, prepErr, code)
				return prepared, options
			}
			for _, mode := range []string{"missing-proof", "forged-checkpoint", "wrong-call", "changed-request", "changed-summary", "changed-session-kind", "foreign-owner", "wrong-run", "wrong-step", "mixed-answer", "wrong-rerun", "changed-child-parent", "changed-child-owner", "changed-checkpoint"} {
				t.Run(mode, func(t *testing.T) {
					rowID, concept := "v1:work:approval:"+memql.BareShortId(wait.ApprovalID), "v1:work:approval"
					if mode == "wrong-rerun" {
						rowID, concept = "v1:work:run:"+runID, "v1:work:run"
					}
					if strings.HasPrefix(mode, "changed-child-") {
						rowID, concept = "v1:work:run:"+memql.BareShortId(childID), "v1:work:run"
					}
					if mode == "changed-checkpoint" {
						rowID, concept = "v1:work:observation:"+memql.BareShortId(checkpoint["id"].(string)), "v1:work:observation"
					}
					var raw string
					require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, rowID).Scan(person, &raw))
					var original, changed map[string]any
					require.NoError(t, json.Unmarshal([]byte(raw), &original))
					require.NoError(t, json.Unmarshal([]byte(raw), &changed))
					s, _ := changed["subject"].(map[string]any)
					switch mode {
					case "missing-proof":
						delete(s, "nativeScopeCheckpoint")
					case "forged-checkpoint":
						s["nativeScopeCheckpoint"].(map[string]any)["continuationId"] = "forged"
					case "wrong-call":
						s["nativeScopeCheckpoint"].(map[string]any)["toolCallId"] = "unstarted"
					case "changed-request":
						s["nativeScopeCheckpoint"].(map[string]any)["requestHash"] = "forged"
					case "changed-summary":
						s["summary"] = "A different request"
					case "changed-session-kind":
						s["sessionKind"] = "http"
					case "foreign-owner", "changed-child-owner":
						changed["ownerUserId"] = "v1:identity:user:other"
					case "wrong-run":
						changed["runId"] = "v1:work:run:other"
					case "wrong-step":
						changed["stepKey"] = "other"
					case "mixed-answer":
						changed["answer"] = map[string]any{"feedback": permissionCorrection, "permissionMode": "always"}
					case "wrong-rerun":
						changed["rerun"] = map[string]any{"requestId": "other"}
					case "changed-child-parent":
						changed["parentRunId"] = "v1:work:run:other"
					case "changed-checkpoint":
						changed["data"].(map[string]any)["messages"] = "changed"
					}
					if s != nil {
						changed["artifactHash"] = workstate.ArtifactHash(s)
					} // Recomputed hashes do not legitimize a forged witness.
					insertWorkRow(t, db, rowID, concept, time.Now().UTC(), changed)
					refused := triggers[1].executor()
					prepared, options := prepare(journal)
					_, err := refused.ResumeFrom(ctx, prepared, triggers[1].template.members["parent"], options)
					refused.Close()
					require.Error(t, err)
					require.Empty(t, runners[1].requests)
					require.Empty(t, runners[1].reviews)
					require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": completedTools, "completed-tool": completedTools}, effects)
					insertWorkRow(t, db, rowID, concept, time.Now().UTC(), original)
				})
			}
			peer := triggers[1].executor()
			defer peer.Close()
			prepared, options := prepare(load())
			done, err := peer.ResumeFrom(ctx, prepared, triggers[1].template.members["parent"], options)
			require.NoError(t, err)
			require.Equal(t, "completed", done.Status)
			require.Len(t, runners[1].requests, 1)
			require.Empty(t, runners[0].reviews, "the paused replica cannot review an unfinished reply")
			require.Len(t, runners[1].reviews, 1)
			require.Contains(t, fmt.Sprint(runners[1].reviews[0]), "completed result retained", "the resumed reply review must receive the completed tool receipt")
			require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": completedTools, "completed-tool": completedTools, "child-tail": 1, "later-section": 1}, effects)
			retained := runners[1].requests[0]
			require.Contains(t, retained, common.ChatMessage{Role: "tool", ToolCallId: "completed", Name: "discoverCapabilities", Content: "completed result retained"})
			final := query(writer, "workRunForOwner", map[string]any{"runId": runID})[0]
			require.Equal(t, float64(3), final["spent"].(map[string]any)["modelCalls"])
			require.Equal(t, float64(2), final["spent"].(map[string]any)["retries"])
			finalApproval := query(writer, "workApprovalForOwner", map[string]any{"approvalId": wait.ApprovalID})[0]
			require.Equal(t, "answered", finalApproval["decision"])
			require.Empty(t, finalApproval["permissionUsedAt"])
			child, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, childID)
			require.NoError(t, err)
			require.Equal(t, "succeeded", child.Status)

		})
	}
}
