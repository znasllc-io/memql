//go:build agent

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agent"
	work "github.com/znasllc-io/memql/integrations/work"
)

type replyAppCheckpointFixture struct {
	calls, formats int
	invalid        bool
	engine         *memql.MemQLEngine
}

func (*replyAppCheckpointFixture) Doors(context.Context, string) ([]memql.AppDoor, error) {
	return []memql.AppDoor{{AppId: "codex", Machines: []memql.AppMachine{{RegistrationId: "fixture", Online: true, LocalStream: true, StructuredResult: true, FollowUps: true}}}}, nil
}
func (*replyAppCheckpointFixture) AppOrder(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *replyAppCheckpointFixture) Call(_ context.Context, req memql.AppCallRequest) (memql.AppCallResult, error) {
	if req.Schema != nil && req.Schema.Name == "work_reply_review" && len(req.Images) == 0 && len(req.Inputs) == 0 {
		return memql.AppCallResult{Content: `{"reply":"Hello from the saved App","missingEvidence":"","completion":{"kind":"information","disposition":"satisfied","gap":"","sourceRefs":[]}}`}, nil
	}
	if f.invalid && req.Schema != nil && req.Schema.Name == "work_outcome_format" && len(req.Images) == 0 && len(req.Inputs) == 0 {
		if req.Model != "gpt-6-astra" || req.Effort != "xhigh" {
			return memql.AppCallResult{}, fmt.Errorf("format helper lost the explicit model or effort: %s/%s", req.Model, req.Effort)
		}
		f.formats++
		return memql.AppCallResult{Content: `{"status":"complete","response":"Hello from the saved App","question":null}`}, nil
	}
	return memql.AppCallResult{}, fmt.Errorf("greeting must use the session door")
}
func (f *replyAppCheckpointFixture) RunStep(ctx context.Context, h memql.AppSessionHandover) (memql.AppSessionOutcome, error) {
	f.calls++
	if f.invalid {
		session := id.NewShortId()
		for _, mutation := range []struct {
			name string
			args map[string]any
		}{
			{"createAppSession", map[string]any{"sessionId": session, "workerId": "fixture-worker", "app": "codex", "kind": "run", "runId": h.RunId, "stepId": h.StepId, "workspace": "/fixture", "prompt": "private operational prompt", "toolScope": h.ToolScope, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
			{"endAppSession", map[string]any{"sessionId": session, "status": "ended", "exitCode": 0, "endedAt": time.Now().UTC().Format(time.RFC3339Nano)}},
		} {
			call, err := parser.RenderCall(mutation.name, mutation.args)
			if err != nil {
				return memql.AppSessionOutcome{}, err
			}
			if _, err = f.engine.Execute(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
				return memql.AppSessionOutcome{}, err
			}
		}
		return memql.AppSessionOutcome{SessionId: session, Content: "Hello from the saved App", ExecutionSurface: "cockpit-app:codex"}, nil
	}
	return memql.AppSessionOutcome{Content: "Hello from the saved App", Result: []byte(`{"terminalTool":"respondToUser","arguments":{"status":"complete","response":"Hello from the saved App"}}`), ExecutionSurface: "cockpit-app:codex"}, nil
}

// Only the App is synthetic. Both Replier lanes, native source/stage writes,
// real greeting review, and a second replica's restore run through the adapter.
func TestWorkDBAppReplySourceSurvivesReplicaChange(t *testing.T) {
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	fixture := &replyAppCheckpointFixture{engine: writer}
	var repliers []*agent.Replier
	for _, e := range []*memql.MemQLEngine{writer, reader} {
		e.SetScopedWorkflowRunner(func(ctx context.Context, name string, args map[string]any, ops map[string]memql.WorkflowOperation) (any, error) {
			return workflowhost.Run(ctx, name, args, workflowhost.Options{AmbientEngine: e, Operations: ops})
		})
		for _, plugin := range memql.RegisteredPlugins() {
			if plugin.Name != "work" && plugin.Name != "library" && plugin.Name != "knowledge" {
				continue
			}
			native, err := plugin.Factory(memql.PluginContext{ResolvePartitionFromContext: func(context.Context) string { return "default" }, Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow, ResolveEmbeddingProvider: func(context.Context, string) (memql.EmbeddingAIProvider, error) {
				return nil, fmt.Errorf("synthetic greeting has no lessons to embed")
			}})
			require.NoError(t, err)
			require.NoError(t, e.RegisterIntegration(native))
		}
		e.SetRunCeilingGuard(work.NewRunCeilings(e, e.Logger))
		e.SetModelCallJournal(work.NewModelJournal(e))
		registry := memql.NewProviderRegistryForTest()
		registry.SetAppInference(fixture)
		registry.SetAppSessionDelegate(fixture)
		rules := memql.NewRuleRegistry()
		require.NoError(t, rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "fixture", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}))
		require.NoError(t, rules.Finalize())
		rtr := router.New(registry, memql.NewPolicyRegistryForTest(map[string][]string{"fixture": {"app:codex"}}), rules, nil, e.Logger)
		e.SetAIResolver(rtr)
		replier, err := agent.NewReplier(&CognitionEngineAdapter{Engine: e}, rtr, e.Logger)
		require.NoError(t, err)
		repliers = append(repliers, replier)
	}
	for _, background := range []bool{false, true} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("background=%v/invalid=%v", background, invalid), func(t *testing.T) {
				fixture.invalid = invalid
				key := id.NewShortId()
				owner, goalID, runID := key+"owner", key+"goal", key+"run"
				person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
				write := func(name string, args map[string]any) {
					templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
				}
				write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Say hello", "origin": "user", "ceilings": map[string]any{"maxModelCalls": 10, "wallClockMs": 600000}})
				write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "fixture", "templateFingerprint": "fixture", "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
				write("createWorkStep", map[string]any{"stepId": key + "reason", "runId": runID, "key": "reason", "seq": 0, "stepType": "builtin", "status": "running", "attempt": 1, "version": 1, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
				ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, StepKey: "reason", Mode: common.RunModeLive, Override: &common.StepOverride{Model: "app:codex:gpt-6-astra", Effort: "xhigh"}})
				msg := &memqlv1.AgentGenerateTurnMsg{RequestId: key, AgentId: "fixture", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "fixture", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "hello"}}}
				if background {
					msg.Hints = map[string]string{agent.ExecutionLaneHintKey: agent.ExecutionLaneBackground}
				}
				before := fixture.calls
				formatsBefore := fixture.formats
				for _, replier := range repliers {
					result, err := replier.Handle(ctx, msg, knowledgeTurnSink{})
					require.NoError(t, err)
					require.Equal(t, "Hello from the saved App", result.FinalText)
				}
				require.Equal(t, before+1, fixture.calls, "peer delivery must reuse completed production")
				if invalid {
					require.Equal(t, formatsBefore+1, fixture.formats, "format recovery cannot repeat on a peer")
				} else {
					require.Equal(t, formatsBefore, fixture.formats)
				}
				source, err := reader.RestoreWorkContinuation(ctx, nil)
				require.NoError(t, err)
				state, err := reader.ReadWorkReplyReview(ctx)
				require.NoError(t, err)
				var pending struct {
					Messages []common.ChatMessage `json:"messages"`
				}
				require.NoError(t, json.Unmarshal(state.Pending, &pending))
				require.Equal(t, source, pending.Messages)
				completed := 0
				for _, message := range source {
					if message.Role != "assistant" {
						continue
					}
					for _, call := range message.ToolCalls {
						if call.Name != "respondToUser" {
							continue
						}
						envelope, err := agent.ParseEnvelope(call.Arguments)
						require.NoError(t, err)
						if envelope.Response == "Hello from the saved App" {
							completed++
						}
					}
				}
				require.Equal(t, 1, completed, "durable source must include the completed response once")
			})
		}
	}
}
