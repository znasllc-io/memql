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
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	workmodel "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agent"
	work "github.com/znasllc-io/memql/integrations/work"
)

type replyAppCheckpointFixture struct {
	calls, formats int
	invalid        bool
	unfinished     bool
	engine         *memql.MemQLEngine
	onFormat       func()
}

func (*replyAppCheckpointFixture) Doors(context.Context, string) ([]memql.AppDoor, error) {
	return []memql.AppDoor{{AppId: "codex", Machines: []memql.AppMachine{{RegistrationId: "fixture", Online: true, LocalStream: true, StructuredResult: true, FollowUps: true}}}}, nil
}
func (*replyAppCheckpointFixture) AppOrder(context.Context, string) ([]string, error) {
	return nil, nil
}
func (f *replyAppCheckpointFixture) Call(_ context.Context, req memql.AppCallRequest) (memql.AppCallResult, error) {
	if req.Schema != nil && req.Schema.Name == "work_reply_review" && len(req.Images) == 0 && len(req.Inputs) == 0 {
		if f.unfinished {
			return memql.AppCallResult{Content: `{"reply":"The retained result needs review","missingEvidence":"","completion":{"kind":"operation","disposition":"unfinished","gap":"The saved receipt is not enough","sourceRefs":[]}}`}, nil
		}
		return memql.AppCallResult{Content: `{"reply":"Hello from the saved App","missingEvidence":"","completion":{"kind":"information","disposition":"satisfied","gap":"","sourceRefs":[]}}`}, nil
	}
	if f.invalid && req.Schema != nil && req.Schema.Name == "work_outcome_format" && len(req.Images) == 0 && len(req.Inputs) == 0 {
		if req.Model != "gpt-6-astra" || req.Effort != "xhigh" {
			return memql.AppCallResult{}, fmt.Errorf("format helper lost the explicit model or effort: %s/%s", req.Model, req.Effort)
		}
		f.formats++
		if f.onFormat != nil {
			f.onFormat()
			f.onFormat = nil
		}
		return memql.AppCallResult{Content: `{"status":"complete","response":"Hello from the saved App","question":null}`}, nil
	}
	return memql.AppCallResult{}, fmt.Errorf("greeting must use the session door")
}

type savedReportingErrorRegistry struct{ err error }

func (s savedReportingErrorRegistry) Execute(_ context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	now := time.Now()
	return &automations.StepResult{StepId: step.ID, Status: "failed", Error: s.err.Error(), StartedAt: now, CompletedAt: now}, s.err
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
	auto, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@template
@journalRequired
automation replyAppCheckpointFixture { reason := builtin checkSavedReport() }`, "reporting-stop.memql")
	require.NoError(t, err)
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
		for _, scenario := range []struct{ invalid, unfinished, guidance bool }{{}, {invalid: true}, {invalid: true, unfinished: true}, {invalid: true, guidance: true}} {
			invalid := scenario.invalid
			t.Run(fmt.Sprintf("background=%v/invalid=%v/unfinished=%v/guidance=%v", background, invalid, scenario.unfinished, scenario.guidance), func(t *testing.T) {
				fixture.invalid, fixture.unfinished = invalid, scenario.unfinished
				fixture.onFormat = nil
				key := id.NewShortId()
				owner, goalID, runID := key+"owner", key+"goal", key+"run"
				person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
				write := func(name string, args map[string]any) {
					templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
				}
				write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Say hello", "origin": "user", "ceilings": map[string]any{"maxModelCalls": 10, "wallClockMs": 600000}})
				write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": auto.Name, "templateFingerprint": auto.DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
				write("createWorkStep", map[string]any{"stepId": runID + "-reason", "runId": runID, "key": "reason", "seq": 0, "stepType": "builtin", "status": "running", "attempt": 1, "version": 1, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
				if scenario.guidance {
					fixture.onFormat = func() {
						approval := id.NewShortId()
						write("createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "reason", "kind": "scopeElevation", "subject": map[string]any{}, "artifactHash": "fixture", "question": "Inspect the inputs?", "requestedAt": time.Now().UTC().Format(time.RFC3339Nano)})
						write("decideWorkApproval", map[string]any{"approvalId": approval, "decision": "answered", "decidedBy": owner, "decidedAt": time.Now().UTC().Format(time.RFC3339Nano), "answer": map[string]any{"feedback": "Leave the greeting for review."}})
					}
				}
				ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, StepKey: "reason", Mode: common.RunModeLive, Override: &common.StepOverride{Model: "app:codex:gpt-6-astra", Effort: "xhigh"}})
				msg := &memqlv1.AgentGenerateTurnMsg{RequestId: key, AgentId: "fixture", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "fixture", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "hello"}}}
				if background {
					msg.Hints = map[string]string{agent.ExecutionLaneHintKey: agent.ExecutionLaneBackground}
					// The native executor marks its whole request, including review
					// helpers, as background; the synthetic entry must do the same.
					ctx = memql.ContextWithBackgroundLane(ctx)
				}
				before := fixture.calls
				formatsBefore := fixture.formats
				var reportingError error
				for replica, replier := range repliers {
					result, err := replier.Handle(ctx, msg, knowledgeTurnSink{})
					if scenario.guidance && replica == 0 {
						require.ErrorContains(t, err, "outcome recovery execution or guidance changed")
					} else if scenario.unfinished {
						require.ErrorContains(t, err, "work_outcome_format_unverified")
						require.NotErrorIs(t, err, memql.ErrWorkCheckpointRequired)
						reportingError = err
					} else {
						require.NoError(t, err)
						require.Equal(t, "Hello from the saved App", result.FinalText)
					}
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
				if scenario.guidance {
					// The executor retains the run-level error while moving to the
					// next step. With no App for that step, it is new production.
					write("updateWorkRun", map[string]any{"runId": runID, "errorMessage": workmodel.OutcomeRecoveryError(fmt.Errorf("previous reporting failure")).Error()})
					write("createWorkStep", map[string]any{"stepId": runID + "-next", "runId": runID, "key": "next", "seq": 1, "stepType": "builtin", "status": "running", "attempt": 1, "version": 1, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
					next, _ := common.RunFromContext(ctx)
					next.StepKey = "next"
					fixture.invalid = false
					msg.RequestId = key + "next"
					result, err := repliers[1].Handle(common.ContextWithRun(ctx, next), msg, knowledgeTurnSink{})
					require.NoError(t, err)
					require.Equal(t, "Hello from the saved App", result.FinalText)
					require.Equal(t, before+2, fixture.calls, "the next step starts exactly one new operational turn")
					require.Equal(t, formatsBefore+1, fixture.formats, "the old step's formatter remains consumed")
				}
				if reportingError != nil {
					// Carry the actual Replier error through the outer executor and
					// required database journal. A semantic stop must not leave the
					// last step running or masquerade as missing durable storage.
					journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
					require.NoError(t, err)
					executor := automations.NewExecutor(automations.ExecutorOptions{Engine: reader, StepRegistry: savedReportingErrorRegistry{reportingError}})
					t.Cleanup(executor.Close)
					// The registry can only return the already-captured error; this
					// fixture explicitly permits retrying that synthetic function.
					_, err = executor.ResumeFrom(person, journal, auto, &automations.ResumeOptions{AllowSideEffects: true})
					require.ErrorContains(t, err, "work_outcome_format_unverified")
					require.NotErrorIs(t, err, automations.ErrJournalRequired)
					journal, err = automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
					require.NoError(t, err)
					require.Equal(t, "waiting", journal.Status, "the installed failure path must create an explicit review wait")
					require.Equal(t, "approval", journal.WaitingOn["kind"])
					require.NotEmpty(t, journal.WaitingOn["subject"])
					require.Equal(t, "failed", journal.StepStates["reason"].Status)
					require.False(t, journal.HasRunningStep)
				}
			})
		}
	}
}
