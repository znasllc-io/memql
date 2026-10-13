//go:build agent

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
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

// The provider is the only failing boundary. Router accounting, real Replier,
// native checkpoint writes, nested journals, human decision and peer resume run.
type ceilingDBProvider struct {
	calls, timeouts int
	requests        [][]common.ChatMessage
	beforeFailure   func()
}

func (*ceilingDBProvider) Call(context.Context, string) (any, error) {
	return nil, fmt.Errorf("unexpected text call")
}
func (p *ceilingDBProvider) CallChatWithTools(ctx context.Context, m []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.calls++
	p.requests = append(p.requests, append([]common.ChatMessage(nil), m...))
	if p.calls == 1 && p.timeouts > 0 {
		return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{{ID: "read", Name: "discoverCapabilities", Arguments: `{}`}}}, nil
	}
	if p.calls > 1 && p.calls <= 1+p.timeouts {
		if p.beforeFailure != nil {
			p.beforeFailure()
		}
		return nil, fmt.Errorf("worker: model call exceeded its 10m0s ceiling")
	}
	return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{{ID: "done", Name: "respondToUser", Arguments: `{"status":"complete","response":"Completed synthetic section"}`}}}, nil
}
func (p *ceilingDBProvider) CallChatStreamWithTools(ctx context.Context, m []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	result, err := p.CallChatWithTools(ctx, m, tools)
	ch := make(chan common.StreamToolChunk, 1)
	if err != nil {
		ch <- common.StreamToolChunk{Error: err}
	} else {
		ds := []common.ToolCallDelta{}
		for n, c := range result.ToolCalls {
			ds = append(ds, common.ToolCallDelta{Index: n, ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
		ch <- common.StreamToolChunk{ToolCalls: ds, Done: true}
	}
	close(ch)
	return ch, nil
}

type ceilingDBTurn struct {
	*installedBudgetTurn
	paused func(string)
}

func (r *ceilingDBTurn) RunTurn(ctx context.Context, m *memqlv1.AgentGenerateTurnMsg) (string, error) {
	answer, err := r.installedBudgetTurn.RunTurn(ctx, m)
	var wait *workstate.HumanWait
	if errors.As(err, &wait) && r.paused != nil {
		r.paused(wait.ApprovalID)
	}
	return answer, err
}

func TestWorkDBModelCeilingKeepsExactNestedProgress(t *testing.T) {
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	for _, background := range []bool{false, true} {
		for _, mode := range []string{"recover", "exhausted", "early-answer", "repeated", "checkpoint-failure", "cancelled", "closed-goal", "policy-ask", "guidance", "abandon"} {
			t.Run(fmt.Sprintf("background=%v/%s", background, mode), func(t *testing.T) {
				var turns []*ceilingDBTurn
				writer, db := workTemplateDBEngineAndDB(t)
				reader := workTemplateDBEngine(t)
				gates, err := database.NewGatePool(dbtest.DSN(), 0)
				require.NoError(t, err)
				defer gates.Close()
				key := strings.ReplaceAll(id.NewShortId(), "-", "")
				owner, runID, goalID := key+"owner", key+"run", key+"goal"
				person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
				run := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive}
				ctx := common.ContextWithRun(person, run)
				effects := map[string]int{}
				reads := 0
				providers := []*ceilingDBProvider{{timeouts: 2}, {}}
				if mode == "recover" {
					providers[0].timeouts = 1
				}
				if mode == "repeated" {
					providers[1].timeouts = 2
				}
				peers := []*work.Integration{}
				triggers := []*workTemplateTrigger{}
				for n, e := range []*memql.MemQLEngine{writer, reader} {
					e.SetScopedWorkflowRunner(func(ctx context.Context, name string, args map[string]any, ops map[string]memql.WorkflowOperation) (any, error) {
						opts := workflowhost.Options{AmbientEngine: e, Operations: ops}
						if mode == "policy-ask" {
							custom, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@template automation workModelCallRecovery {
args { retriesSpent int! maxRetries int! }
return { act: "ask", question: "A stricter authored policy requires a person before retrying." }
}`, "stricter-model-recovery.memql")
							if err != nil {
								return nil, err
							}
							opts.Load = func(string) (*automations.Automation, error) { return custom, nil }
						}
						return workflowhost.Run(ctx, name, args, opts)
					})
					var wi *work.Integration
					for _, plugin := range memql.RegisteredPlugins() {
						if plugin.Name != "work" && plugin.Name != "library" {
							continue
						}
						provider, err := plugin.Factory(memql.PluginContext{Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow, ResolveEmbeddingProvider: func(context.Context, string) (memql.EmbeddingAIProvider, error) {
							return nil, fmt.Errorf("synthetic work has no owned Training lessons to embed")
						}})
						require.NoError(t, err)
						require.NoError(t, e.RegisterIntegration(provider))
						if plugin.Name == "work" {
							wi = provider.(*work.Integration)
						}
					}
					require.NotNil(t, wi)
					peers = append(peers, wi)
					e.SetRunCeilingGuard(work.NewRunCeilings(e, e.Logger))
					e.SetModelCallJournal(work.NewModelJournal(e))
					registry := memql.NewProviderRegistryForTest()
					registry.RegisterWithParamsForTest("fixture", "OpenAI", "fixture", map[string]any{"contextWindow": 200000}, providers[n])
					rules := memql.NewRuleRegistry()
					require.NoError(t, rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "fixture", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}))
					require.NoError(t, rules.Finalize())
					rtr := router.New(registry, memql.NewPolicyRegistryForTest(map[string][]string{"fixture": {"fixture"}}), rules, nil, e.Logger)
					adapter := &modelBudgetAdapter{CognitionEngineAdapter: &CognitionEngineAdapter{Engine: e}, reads: &reads}
					replier, err := agent.NewReplier(adapter, rtr, e.Logger)
					require.NoError(t, err)
					ai := agents.New(e.Agents(), e)
					turn := &ceilingDBTurn{installedBudgetTurn: &installedBudgetTurn{replierTurnRunner: &replierTurnRunner{replier: replier, engine: e}, background: background}}
					turns = append(turns, turn)
					ai.SetAgentTurnRunner(turn)
					require.NoError(t, e.RegisterIntegration(ai))
					require.NoError(t, e.RegisterIntegration(&childPauseFixture{engine: e, t: t, effects: effects}))
					require.NoError(t, e.Functions().Upsert(&memql.Function{Name: "fixtureeffect", Type: memql.FunctionTypeBuiltin, Enabled: true, BuiltinArgs: &memql.BuiltinArgContract{Profile: memql.BuiltinArgProfileOptionalObject, Properties: map[string]string{"label": "string"}}, ArgsSchema: &memql.ArgsSchemaConfig{Fields: []*memql.FunctionArgsField{{Name: "label", Type: "string"}}}, FunctionKind: "builtin", Executor: "integration.childPauseFixture.effect"}))
					loader := automations.NewLoader(automations.LoaderOptions{Functions: e.Functions()})
					tpl := &workTemplate{members: map[string]*automations.Automation{}}
					for _, src := range []string{
						`@template automation section { builtin fixtureeffect(label: "child-prefix"); answer := builtin runAgentTurn(prompt: "Complete the synthetic section") retry(2) on error continue; builtin fixtureeffect(label: "child-tail"); return answer }`,
						`@template automation parent { builtin fixtureeffect(label: "completed-section"); answer := automation section(); builtin fixtureeffect(label: "later-section"); return answer }`,
					} {
						a, err := loader.CompileSource(strings.ReplaceAll(src, "; ", "\n"), "model-budget.memql")
						require.NoError(t, err)
						tpl.members[a.Name] = a
					}
					triggers = append(triggers, &workTemplateTrigger{app: &App{engine: e, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: e.Logger}, template: tpl})
				}

				write := func(name string, args map[string]any) {
					templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
				}
				write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Complete a synthetic section", "origin": "user", "ceilings": map[string]any{"maxModelCalls": 30, "maxRetries": 3, "wallClockMs": 10800000}})
				write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": triggers[0].template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
				write("updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"retries": 2, "events": 7}})
				if mode == "checkpoint-failure" {
					name := "modelcheckpoint" + strings.ToLower(key)
					_, err := db.ExecContext(person, `CREATE FUNCTION `+name+`() RETURNS trigger AS $$ BEGIN IF NEW.concept='v1:work:observation' AND NEW.payload->>'runId'='v1:work:run:`+runID+`' AND NEW.payload->'data'->>'continuation'='true' THEN RAISE EXCEPTION 'synthetic checkpoint write refusal'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`)
					require.NoError(t, err)
					_, err = db.ExecContext(person, `CREATE TRIGGER `+name+` BEFORE INSERT ON "MemoryNodes" FOR EACH ROW EXECUTE FUNCTION `+name+`()`)
					require.NoError(t, err)
					t.Cleanup(func() {
						_, _ = db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON "MemoryNodes"`)
						_, _ = db.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
					})
				}
				if mode == "cancelled" || mode == "closed-goal" {
					providers[0].beforeFailure = func() {
						if mode == "cancelled" {
							write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true})
						} else {
							write("updateWorkGoal", map[string]any{"goalId": goalID, "status": "closed"})
						}
					}
				}
				const guidance = "Use the completed read as evidence, then finish the remaining response without repeating it."
				decide := func(approval string, value string) {
					answer := map[string]any{"value": value}
					if value == "guidance" {
						answer = map[string]any{"feedback": guidance}
					}
					for _, cap := range peers[1].Capabilities() {
						if cap.Name == "decideApproval" {
							_, err := cap.Handler(person, map[string]any{"approvalId": approval, "decision": "answered", "answer": answer}, 0)
							require.NoError(t, err)
							return
						}
					}
					t.Fatal("missing decision capability")
				}
				if mode == "early-answer" {
					turns[0].paused = func(approval string) {
						decide(approval, "retry")
						_, ready := (&workRunDispatcher{app: &App{engine: reader, Logger: reader.Logger}}).PrepareDispatch(person, work.DispatchRequest{RunId: runID, HumanResumeId: approval})
						require.False(t, ready, "wait for exact nested receipts before dispatch")
					}
				}
				ex := triggers[0].executor()
				defer ex.Close()
				done, err := ex.ExecuteAdopted(ctx, triggers[0].template.members["parent"], automations.RunAdoption{RunId: runID})
				if mode == "checkpoint-failure" || mode == "cancelled" || mode == "closed-goal" {
					require.Error(t, err, "effects=%v calls=%d done=%+v", effects, providers[0].calls, done)
					if mode == "checkpoint-failure" {
						require.ErrorIs(t, err, automations.ErrJournalRequired)
					}
					require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1}, effects)
					var questions int
					require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:approval' AND payload->>'ownerUserId' IN (?,?)`, owner, "v1:identity:user:"+owner).Scan(person, &questions))
					require.Zero(t, questions)
					return
				}
				if mode == "recover" {
					require.NoError(t, err)
					require.Equal(t, "completed", done.Status)
					require.Equal(t, 3, providers[0].calls)
				} else {
					var wait *workstate.HumanWait
					require.ErrorAs(t, err, &wait)
					require.Equal(t, 1, reads)
					require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1}, effects)
					if mode == "policy-ask" {
						require.Equal(t, 2, providers[0].calls)
					} else {
						require.Equal(t, 3, providers[0].calls)
					}
					journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
					require.NoError(t, err)
					childID := journal.StepStates[journal.FailedStep].ChildRunId
					require.NotEmpty(t, childID)
					child, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, childID)
					require.NoError(t, err)
					require.Equal(t, "waiting", child.Status)
					if mode == "guidance" {
						call, err := parser.RenderCall("askApprovalDiscussion", map[string]any{"approvalId": wait.ApprovalID})
						require.NoError(t, err)
						discussion, err := reader.Execute(person, "builtin "+call)
						require.NoError(t, err)
						rows := memql.MaterializeRows(discussion.OutputPayload())
						require.Len(t, rows, 1)
						require.Equal(t, true, rows[0]["canSendGuidance"])
						decide(wait.ApprovalID, "guidance")
					} else if mode == "abandon" {
						decide(wait.ApprovalID, "abandon")
						journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
						require.NoError(t, err)
						require.Equal(t, "failed", journal.Status, "Abandon terminates the root instead of leaving another recoverable wait")
						_, ready := (&workRunDispatcher{app: triggers[1].app}).PrepareDispatch(person, work.DispatchRequest{RunId: runID, HumanResumeId: wait.ApprovalID})
						require.False(t, ready)
						require.Zero(t, providers[1].calls)
						require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1}, effects)
						return
					} else if mode != "early-answer" {
						decide(wait.ApprovalID, "retry")
					}
					journal, err = automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
					require.NoError(t, err)
					require.Nil(t, journal.Rerun)
					require.Equal(t, "running", journal.Status)
					_, ready := (&workRunDispatcher{app: triggers[1].app}).PrepareDispatch(person, work.DispatchRequest{RunId: runID, HumanResumeId: wait.ApprovalID})
					require.True(t, ready)

					if mode == "exhausted" {
						approvalID := "v1:work:approval:" + wait.ApprovalID
						var payload string
						require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, approvalID).Scan(person, &payload))
						var original map[string]any
						require.NoError(t, json.Unmarshal([]byte(payload), &original))
						for _, negative := range []string{"missing-binding", "changed-checkpoint", "pending", "wrong-step", "foreign-owner", "wrong-answer", "mixed-answer", "empty-guidance"} {
							var changed map[string]any
							require.NoError(t, json.Unmarshal([]byte(payload), &changed))
							switch negative {
							case "missing-binding":
								delete(changed["subject"].(map[string]any), "resumeKind")
							case "changed-checkpoint":
								changed["subject"].(map[string]any)["continuationId"] = "foreign-checkpoint"
							case "pending":
								delete(changed, "decision")
							case "wrong-step":
								changed["stepKey"] = "another-step"
							case "foreign-owner":
								changed["ownerUserId"] = "v1:identity:user:another-owner"
							case "wrong-answer":
								changed["answer"] = map[string]any{"value": "abandon"}
							case "mixed-answer":
								changed["answer"] = map[string]any{"value": "retry", "feedback": guidance}
							case "empty-guidance":
								changed["answer"] = map[string]any{"feedback": " "}
							}
							insertWorkRow(t, db, approvalID, "v1:work:approval", time.Now().UTC(), changed)
							refused := triggers[1].executor()
							_, err := refused.ResumeFrom(ctx, journal, triggers[1].template.members["parent"], nil)
							refused.Close()
							require.Error(t, err, negative)
							require.Zero(t, providers[1].calls)
							insertWorkRow(t, db, approvalID, "v1:work:approval", time.Now().UTC(), original)
						}
					}
					resumed := triggers[1].executor()
					defer resumed.Close()
					done, err = resumed.ResumeFrom(ctx, journal, triggers[1].template.members["parent"], nil)
					if mode == "repeated" {
						var second *workstate.HumanWait
						require.ErrorAs(t, err, &second)
						require.NotEqual(t, wait.ApprovalID, second.ApprovalID)
						decide(second.ApprovalID, "retry")
						journal, err = automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
						require.NoError(t, err)
						done, err = resumed.ResumeFrom(ctx, journal, triggers[1].template.members["parent"], nil)
					}
					require.NoError(t, err)
					require.Equal(t, "completed", done.Status)
					finalChild, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), writer, childID)
					require.NoError(t, err)
					require.Equal(t, "succeeded", finalChild.Status)
					found, guided := false, false
					for _, m := range providers[1].requests[0] {
						if strings.HasPrefix(m.Content, "[Recorded answer for this ") && strings.Contains(m.Content, guidance) {
							guided = true
						}
						if m.Role == "tool" && m.ToolCallId == "read" {
							require.Contains(t, m.Content, "read completed once")
							found = true
						}
					}
					require.True(t, found, "peer model sees completed tool receipt")
					if mode == "guidance" {
						require.True(t, guided, "peer model receives the human feedback with its saved exchange")
					}
				}
				require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1, "child-tail": 1, "later-section": 1}, effects)
				if mode != "repeated" {
					require.Equal(t, 1, reads)
				}
				var raw string
				require.NoError(t, db.NewRaw(`SELECT payload->'spent' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(person, &raw))
				require.Contains(t, raw, `"retries": 2`)
				require.Contains(t, raw, `"events": 7`)
				var modelCalls int
				require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:modelCall' AND payload->>'runId'=?`, "v1:work:run:"+runID).Scan(person, &modelCalls))
				require.Equal(t, providers[0].calls+providers[1].calls, modelCalls, "each provider attempt stays charged")
			})
		}
	}
}
