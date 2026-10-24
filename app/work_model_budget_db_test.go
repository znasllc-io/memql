//go:build agent

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
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
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agent"
	"github.com/znasllc-io/memql/integrations/agents"
	work "github.com/znasllc-io/memql/integrations/work"
)

// Only model answers and one harmless tool are controlled. Production router
// observers, budget arithmetic, both replier loops, the outer runner, nested
// journals, approval and peer continuation all execute their installed code.
type modelBudgetProvider struct {
	calls     int
	reviews   int
	readFirst bool
	requests  [][]common.ChatMessage
}

func (p *modelBudgetProvider) CallChatStructured(ctx context.Context, _ []common.ChatMessage, _ common.StructuredSchema) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.reviews++
	return `{"reply":"Synthetic section complete"}`, nil
}
func (p *modelBudgetProvider) Call(context.Context, string) (any, error) {
	return nil, fmt.Errorf("unexpected text call")
}
func (p *modelBudgetProvider) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, _ []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.calls++
	p.requests = append(p.requests, append([]common.ChatMessage(nil), messages...))
	if p.readFirst && p.calls == 1 {
		return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{{ID: "read", Name: "discoverCapabilities", Arguments: `{}`}}}, nil
	}
	return &common.ToolCallingChatResult{ToolCalls: []common.ToolCall{{ID: "done", Name: "respondToUser", Arguments: `{"status":"complete","response":"Synthetic section complete"}`}}}, nil
}
func (p *modelBudgetProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
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

type modelBudgetAdapter struct {
	*CognitionEngineAdapter
	reads          *int
	failCheckpoint bool
	legacyFailure  bool
	legacyCeiling  *memql.RunCeilingError
}

func (a *modelBudgetAdapter) ToolDefinitionsForNames(names []string) []common.ToolDefinition {
	if slices.Contains(names, "discoverCapabilities") {
		return []common.ToolDefinition{{Name: "discoverCapabilities", Description: "Read synthetic evidence", InputSchema: map[string]any{"type": "object"}}}
	}
	return nil
}
func (a *modelBudgetAdapter) ExecuteToolByName(_ context.Context, name string, _ map[string]any) (string, error) {
	if name != "discoverCapabilities" {
		return "", fmt.Errorf("unexpected tool %s", name)
	}
	*a.reads++
	return `{"evidence":"read completed once"}`, nil
}
func (a *modelBudgetAdapter) PauseWorkForBudget(ctx context.Context, c *memql.RunCeilingError, m []common.ChatMessage) error {
	if a.legacyFailure {
		a.legacyCeiling = c
		return c
	}
	if a.failCheckpoint {
		return fmt.Errorf("%w: synthetic checkpoint failure", memql.ErrWorkCheckpointRequired)
	}
	return a.CognitionEngineAdapter.PauseWorkForBudget(ctx, c, m)
}

type installedBudgetTurn struct {
	*replierTurnRunner
	background bool
	legacy     *modelBudgetAdapter
}

func (r *installedBudgetTurn) ResolveOwnerAgent(context.Context, string) (string, error) {
	return "fixture-agent", nil
}
func (r *installedBudgetTurn) RunTurn(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) (string, error) {
	if r.background {
		if msg.Hints == nil {
			msg.Hints = map[string]string{}
		}
		msg.Hints[agent.ExecutionLaneHintKey] = "background"
	}
	answer, err := r.replierTurnRunner.RunTurn(ctx, msg)
	if r.legacy != nil && r.legacy.legacyCeiling != nil {
		return "", r.legacy.legacyCeiling
	}
	return answer, err
}

// These scenarios control the point where an estimate is reached, while the
// installed guard still accounts for all model receipts and enforces hard caps.
// Automatic extension policy is covered with real stored estimates in work's
// cross-replica progress tests. This fixture isolates checkpoint delivery.
type checkpointEstimateGuard struct {
	*work.RunCeilings
	calls *int
	wall  *int64
	start *time.Time
}

func (g *checkpointEstimateGuard) Admit(ctx context.Context, rc common.RunContext, tokens int) *memql.RunCeilingBreach {
	if breach := g.RunCeilings.Admit(ctx, rc, tokens); breach != nil {
		return breach
	}
	used := g.Spent(ctx, rc)
	if *g.calls > 0 && used.ModelCalls >= *g.calls {
		return &memql.RunCeilingBreach{Ceiling: "modelCalls", Limit: fmt.Sprint(*g.calls), Actual: fmt.Sprint(used.ModelCalls), Reason: "synthetic soft call estimate"}
	}
	if *g.wall > 0 && used.WallClockMs >= *g.wall {
		return &memql.RunCeilingBreach{Ceiling: "wallClock", Limit: fmt.Sprint(*g.wall), Actual: fmt.Sprint(used.WallClockMs), Reason: "synthetic soft time estimate"}
	}
	return nil
}
func (g *checkpointEstimateGuard) Deadline(ctx context.Context, rc common.RunContext) (time.Time, error) {
	hard, err := g.RunCeilings.Deadline(ctx, rc)
	if err != nil {
		return hard, err
	}
	soft := g.start.Add(time.Duration(*g.wall) * time.Millisecond)
	if *g.wall > 0 && (hard.IsZero() || soft.Before(hard)) {
		return soft, nil
	}
	return hard, nil
}
func (g *checkpointEstimateGuard) DeclaredDeadline(ctx context.Context, rc common.RunContext) (time.Time, error) {
	return g.Deadline(ctx, rc)
}

func TestWorkDBModelAdmissionBudgetKeepsOneNestedCheckpoint(t *testing.T) {
	testModelBudgetBoundaries(t, []string{"initial", "after-tool", "checkpoint-failure"})
}
func TestWorkDBFinalReviewBudgetResumesOnAnotherReplica(t *testing.T) {
	testModelBudgetBoundaries(t, []string{"reply-review"})
}
func TestWorkDBProgressGuidanceSurvivesRepeatedCheckpoints(t *testing.T) {
	testModelBudgetBoundaries(t, []string{"guidance"})
}
func TestWorkDBNativeBudgetContinuesTheExplicitRerun(t *testing.T) {
	testModelBudgetBoundaries(t, []string{"legacy-rerun", "repair-rerun", "model-rerun"})
}
func TestWorkDBEntryBudgetContinuesTheExplicitRerun(t *testing.T) {
	testModelBudgetBoundaries(t, []string{"entry-rerun", "entry-rerun-failure", "entry-rerun-tail", "model-rerun-tail"})
}
func testModelBudgetBoundaries(t *testing.T, boundaries []string) {
	for _, background := range []bool{false, true} {
		for _, boundary := range boundaries {
			tail := boundary == "entry-rerun-tail" || boundary == "model-rerun-tail"
			entry := strings.HasPrefix(boundary, "entry-rerun") && !tail
			rerun := boundary == "legacy-rerun" || boundary == "repair-rerun" || boundary == "model-rerun" || entry || tail
			t.Run(fmt.Sprintf("background=%v/%s", background, boundary), func(t *testing.T) {
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
				estimatedCalls, estimatedWall := 1, int64(10800000)
				if boundary == "reply-review" {
					estimatedCalls = 2
				}
				started := time.Now()
				providers := []*modelBudgetProvider{{readFirst: boundary != "initial"}, {}}
				peers := []*work.Integration{}
				triggers := []*workTemplateTrigger{}
				for n, e := range []*memql.MemQLEngine{writer, reader} {
					var wi *work.Integration
					for _, plugin := range memql.RegisteredPlugins() {
						if plugin.Name != "work" && plugin.Name != "library" && plugin.Name != "knowledge" {
							continue
						}
						provider, err := plugin.Factory(memql.PluginContext{ResolvePartitionFromContext: func(context.Context) string { return "default" }, Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow, ResolveEmbeddingProvider: func(context.Context, string) (memql.EmbeddingAIProvider, error) {
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
					e.SetRunCeilingGuard(&checkpointEstimateGuard{RunCeilings: work.NewRunCeilings(e, e.Logger), calls: &estimatedCalls, wall: &estimatedWall, start: &started})
					e.SetModelCallJournal(work.NewModelJournal(e))
					registry := memql.NewProviderRegistryForTest()
					registry.RegisterWithParamsForTest("fixture", "OpenAI", "fixture", map[string]any{"contextWindow": 200000}, providers[n])
					rules := memql.NewRuleRegistry()
					require.NoError(t, rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "fixture", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}))
					require.NoError(t, rules.Finalize())
					rtr := router.New(registry, memql.NewPolicyRegistryForTest(map[string][]string{"fixture": {"fixture"}}), rules, nil, e.Logger)
					e.SetAIResolver(rtr)
					adapter := &modelBudgetAdapter{CognitionEngineAdapter: &CognitionEngineAdapter{Engine: e}, reads: &reads, failCheckpoint: (n == 0 && boundary == "checkpoint-failure") || (n == 1 && boundary == "entry-rerun-failure"), legacyFailure: n == 0 && rerun}
					replier, err := agent.NewReplier(adapter, rtr, e.Logger)
					require.NoError(t, err)
					ai := agents.New(e.Agents(), e)
					ai.SetAgentTurnRunner(&installedBudgetTurn{replierTurnRunner: &replierTurnRunner{replier: replier, engine: e}, background: background, legacy: adapter})
					require.NoError(t, e.RegisterIntegration(ai))
					require.NoError(t, e.RegisterIntegration(&childPauseFixture{engine: e, t: t, effects: effects}))
					require.NoError(t, e.Functions().Upsert(&memql.Function{Name: "fixtureeffect", Type: memql.FunctionTypeBuiltin, Enabled: true, BuiltinArgs: &memql.BuiltinArgContract{Profile: memql.BuiltinArgProfileOptionalObject, Properties: map[string]string{"label": "string"}}, ArgsSchema: &memql.ArgsSchemaConfig{Fields: []*memql.FunctionArgsField{{Name: "label", Type: "string"}}}, FunctionKind: "builtin", Executor: "integration.childPauseFixture.effect"}))
					loader := automations.NewLoader(automations.LoaderOptions{Functions: e.Functions()})
					tpl := &workTemplate{members: map[string]*automations.Automation{}}
					for _, src := range []string{
						`@template automation section { builtin fixtureeffect(label: "child-prefix"); answer := builtin runAgentTurn(prompt: "Complete the synthetic section") retry(2) on error continue; builtin fixtureeffect(label: "child-tail"); return answer }`,
						`@template automation parent { builtin fixtureeffect(label: "completed-section"); answer := automation section(); builtin fixtureeffect(label: "later-section"); return answer }`,
					} {
						if rerun {
							src = strings.ReplaceAll(src, " retry(2) on error continue", "")
						}
						a, err := loader.CompileSource(strings.ReplaceAll(src, "; ", "\n"), "model-budget.memql")
						require.NoError(t, err)
						tpl.members[a.Name] = a
					}
					triggers = append(triggers, &workTemplateTrigger{app: &App{engine: e, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: e.Logger}, template: tpl})
				}
				write := func(name string, args map[string]any) {
					templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
				}
				started = time.Now()
				if entry || tail {
					started = started.Add(-2 * time.Hour)
				}
				write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic section boundary", "origin": "user", "ceilings": map[string]any{"maxModelCalls": 96, "wallClockMs": 10800000}})
				write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": triggers[0].template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": started.UTC().Format(time.RFC3339Nano)})
				if boundary == "initial" {
					require.NoError(t, work.NewModelJournal(writer).Record(person, owner, memql.JournaledCall{RunId: runID, StepKey: "prior", RequestHash: "prior-section", PromptRef: "fixture", Served: memql.ServedLocal}))
				}
				ex := triggers[0].executor()
				defer ex.Close()
				_, err = ex.ExecuteAdopted(ctx, triggers[0].template.members["parent"], automations.RunAdoption{RunId: runID})
				var count int
				require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:approval' AND payload->>'ownerUserId' IN (?,?)`, owner, "v1:identity:user:"+owner).Scan(person, &count))
				if boundary == "checkpoint-failure" {
					require.ErrorIs(t, err, automations.ErrJournalRequired)
					require.Zero(t, count)
					require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1}, effects)
					require.Equal(t, 1, reads)
					return
				}
				load := func(e *memql.MemQLEngine, r string) *automations.RunJournal {
					j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), e, r)
					require.NoError(t, err)
					return j
				}
				act := func(name string, args map[string]any) {
					for _, cap := range peers[1].Capabilities() {
						if cap.Name == name {
							_, err := cap.Handler(person, args, 0)
							require.NoError(t, err)
							return
						}
					}
					t.Fatalf("missing capability %s", name)
				}
				var wait *workstate.HumanWait
				if rerun {
					// Reproduce the stored old shape through the old raw refusal,
					// then use only the existing explicit human decision + rerun APIs.
					require.Error(t, err)
					require.Equal(t, 2, count, "legacy failure: %v", err)
					legacy := load(reader, runID)
					require.Equal(t, "failed", legacy.StepStates[legacy.FailedStep].Status)
					require.Empty(t, legacy.StepStates[legacy.FailedStep].ChildRunId)
					var approvalIDs []string
					require.NoError(t, db.NewRaw(`SELECT DISTINCT id FROM "MemoryNodes" WHERE concept='v1:work:approval' AND payload->>'ownerUserId' IN (?,?)`, owner, "v1:identity:user:"+owner).Scan(person, &approvalIDs))
					for _, approvalID := range approvalIDs {
						act("decideApproval", map[string]any{"approvalId": approvalID, "decision": "rejected"})
					}
					require.Equal(t, "failed", load(reader, runID).Status)
					// A prior answered suspension remains in the run's history.
					// New public rerun intake must not dispatch under that old answer.
					write("updateWorkRun", map[string]any{"runId": runID, "humanResumeId": "previously-consumed-answer"})
					if entry {
						// Real durable usage is already above the cap, and the declared
						// deadline is already past before the next turn can prepare.
						require.NoError(t, work.NewModelJournal(writer).Record(person, owner, memql.JournaledCall{RunId: runID, StepKey: "prior", RequestHash: "another-completed-call", PromptRef: "fixture", Served: memql.ServedLocal}))
						estimatedCalls, estimatedWall = 1, 3600000
					}
					rerunArgs := map[string]any{"runId": runID, "stepKey": legacy.FailedStep}
					if boundary == "model-rerun" || boundary == "model-rerun-tail" {
						rerunArgs["level"] = "strong"
					}
					act("rerunStep", rerunArgs)
					if boundary == "repair-rerun" {
						// Automatic contract repair persists this same guidance on the request.
						repaired := load(reader, runID)
						require.NotNil(t, repaired.Rerun)
						require.False(t, repaired.Rerun.RequestedAt.IsZero(), "public rerun intake must record its guidance chronology")
						// Preserve the saved intake timestamp, not a new clock value or
						// the earlier approval's decision time: this is the same attempt.
						recorded := map[string]any{
							"requestId":   repaired.Rerun.RequestId,
							"requestedAt": repaired.Rerun.RequestedAt.Format(time.RFC3339Nano),
							"stepKey":     repaired.Rerun.StepKey,
							"reason":      "rerun",
							"versions":    repaired.Rerun.Versions,
							"override":    map[string]any{"guidance": map[string]any{"reason": "The prior turn did not produce a valid completion or durable question."}},
						}
						write("updateWorkRun", map[string]any{"runId": runID, "rerun": recorded})
						reloaded := load(reader, runID)
						require.NotNil(t, reloaded.Rerun)
						require.Equal(t, repaired.Rerun.RequestId, reloaded.Rerun.RequestId)
						require.Equal(t, repaired.Rerun.RequestedAt, reloaded.Rerun.RequestedAt, "guidance keeps its original place when another replica resumes it")
					}
					j := load(reader, runID)
					require.NotNil(t, j.Rerun)
					require.Empty(t, j.HumanResumeId)
					initial := work.DispatchRequest{RunId: runID, OwnerUserId: owner, GoalId: goalID, Status: "running", RerunRequestId: j.Rerun.RequestId, HumanResumeId: j.HumanResumeId}
					initial, ready := (&workRunDispatcher{app: &App{engine: reader, Logger: reader.Logger}}).PrepareDispatch(person, initial)
					require.True(t, ready)
					require.True(t, workRerunServable(initial, j))
					prepared, options, prepErr := automations.PrepareRerun(j, nil, triggers[1].template.members["parent"])
					require.NoError(t, prepErr)
					retry := triggers[1].executor()
					defer retry.Close()
					retryCtx, contextErr := workExecutionContext(person, j, nil, nil)
					require.NoError(t, contextErr)
					_, err = retry.ResumeFrom(retryCtx, prepared, triggers[1].template.members["parent"], options)
					if boundary == "entry-rerun-failure" {
						require.ErrorIs(t, err, automations.ErrJournalRequired)
						require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2}, effects)
						require.Zero(t, providers[1].calls)
						require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:approval' AND payload->>'ownerUserId' IN (?,?)`, owner, "v1:identity:user:"+owner).Scan(person, &count))
						require.Equal(t, 2, count, "failed checkpoint must not manufacture another raw question")
						return
					}
					require.ErrorAs(t, err, &wait)
					require.Equal(t, 1, effects["completed-section"], "explicit retry preserves the completed prefix")
					require.Equal(t, 2, effects["child-prefix"], "only the explicitly selected section starts a new version")
					require.Zero(t, providers[1].calls, "original spend must refuse before a provider or another tool")
					fresh := load(reader, runID)
					require.Equal(t, "waiting", fresh.StepStates[fresh.FailedStep].Status)
					require.NotEmpty(t, fresh.StepStates[fresh.FailedStep].ChildRunId)
					// The legacy question was not reused or guessed into native proof.
					require.NotContains(t, approvalIDs, "v1:work:approval:"+wait.ApprovalID)
					require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:approval' AND payload->>'ownerUserId' IN (?,?)`, owner, "v1:identity:user:"+owner).Scan(person, &count))
					require.Equal(t, 3, count, "two legacy questions plus exactly one native root question")
				} else {
					require.ErrorAs(t, err, &wait)
					require.Equal(t, 1, count, "every journal must reuse the same native question")
				}
				journal := load(reader, runID)
				require.Equal(t, "waiting", journal.Status)
				suspended := journal.StepStates[journal.FailedStep]
				require.Equal(t, "waiting", suspended.Status)
				require.NotEmpty(t, suspended.ChildRunId)
				child := load(reader, suspended.ChildRunId)
				require.Equal(t, "waiting", child.Status)
				require.Equal(t, wait.ApprovalID, memql.BareShortId(fmt.Sprint(child.WaitingOn["subject"])))
				expectedReads := 0
				if boundary == "after-tool" || boundary == "guidance" || boundary == "reply-review" || rerun {
					expectedReads = 1
				}
				require.Equal(t, expectedReads, reads)
				initialProviderCalls := expectedReads
				if boundary == "reply-review" {
					initialProviderCalls++
				}
				require.Equal(t, initialProviderCalls, providers[0].calls, "refused model admission must never execute another provider")
				const firstGuidance = "Use the completed evidence; retain the original request and avoid repeating that read."
				const secondGuidance = "Keep the earlier correction and finish the remaining response now."
				sendGuidance := func(text string) {
					act("decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "answered", "answer": map[string]any{"feedback": text}})
					require.Equal(t, "running", load(reader, runID).Status)
				}
				limit := 8
				if boundary == "guidance" {
					limit = 2
					providers[1].readFirst = true
					require.Zero(t, providers[1].calls, "no provider executes before the feedback decision")
				}
				if rerun && !entry {
					limit = 2
					providers[1].readFirst = true
				}
				estimatedCalls = limit
				if boundary == "guidance" {
					sendGuidance(firstGuidance)
				} else {
					act("decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "answered", "answer": map[string]any{"value": "retry"}})
				}
				journal = load(reader, runID)
				require.Equal(t, "running", journal.Status)
				if !rerun {
					require.Nil(t, journal.Rerun)
				}
				admitted := make(chan work.DispatchRequest, 2)
				for n, p := range peers {
					p.SetDispatcher(&humanReadyDispatcher{workRunDispatcher: &workRunDispatcher{app: triggers[n].app}, admitted: admitted})
					p.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer())
				}
				require.True(t, peers[0].DispatchRun(person, runID, owner))
				peers[1].DispatchRun(person, runID, owner)
				select {
				case req := <-admitted:
					require.Equal(t, wait.ApprovalID, req.HumanResumeId)
				case <-time.After(3 * time.Second):
					t.Fatal("budget continuation not dispatched")
				}
				select {
				case <-admitted:
					t.Fatal("duplicate answer dispatched")
				default:
				}
				resume := triggers[1].executor()
				defer resume.Close()
				resumeCtx := ctx
				var options *automations.ResumeOptions
				if rerun {
					// A new replica reconstructs lineage from the persisted row, with none
					// of the first executor's local context or pending-call state.
					var contextErr error
					resumeCtx, contextErr = workExecutionContext(person, journal, nil, nil)
					require.NoError(t, contextErr)
					rc, ok := common.RunFromContext(resumeCtx)
					require.True(t, ok)
					require.Equal(t, journal.Rerun.RequestId, rc.RerunRequestId)
					var prepareErr error
					journal, options, prepareErr = automations.PrepareRerun(journal, nil, triggers[1].template.members["parent"])
					require.NoError(t, prepareErr)
				}
				if rerun {
					// Each rejected admission must leave the suspended effects untouched.
					for _, change := range []string{"missing answer", "missing context lineage", "changed context lineage", "missing request lineage", "changed request lineage", "override", "snapshot", "missing child"} {
						t.Run(change, func(t *testing.T) {
							bad := *journal
							spec := *journal.Rerun
							bad.Rerun = &spec
							badOpts := *options
							badOpts.Rerun = &spec
							badCtx := resumeCtx
							switch change {
							case "missing answer":
								bad.HumanResumeId = ""
							case "missing context lineage", "changed context lineage":
								rc, _ := common.RunFromContext(badCtx)
								rc.RerunRequestId = ""
								if change == "changed context lineage" {
									rc.RerunRequestId = "another"
								}
								badCtx = common.ContextWithRun(badCtx, rc)
							case "missing request lineage", "changed request lineage":
								spec.RequestId = ""
								if change == "changed request lineage" {
									spec.RequestId = "another"
								}
							case "override":
								spec.Override = &common.StepOverride{Prompt: "Changed input"}
							case "snapshot":
								// An empty snapshot changes no workspace. Exercise the
								// forbidden replacement without weakening that distinction.
								spec.Snapshot = &common.WorkspaceSnapshot{Files: []common.SnapshotFile{{Path: "changed.txt", FileId: "v1:library:file:replacement"}}}
							case "missing child":
								bad.StepStates = map[string]automations.StepState{}
								for k, v := range journal.StepStates {
									bad.StepStates[k] = v
								}
								state := bad.StepStates[options.FromStep]
								state.ChildRunId = ""
								bad.StepStates[options.FromStep] = state
							}
							_, err := resume.ResumeFrom(badCtx, &bad, triggers[1].template.members["parent"], &badOpts)
							require.ErrorIs(t, err, automations.ErrNonRetryableStep)
							require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2}, effects)
							require.Zero(t, providers[1].calls)
						})
					}
				}
				done, err := resume.ResumeFrom(resumeCtx, journal, triggers[1].template.members["parent"], options)
				if boundary == "guidance" {
					var again *workstate.HumanWait
					require.ErrorAs(t, err, &again)
					require.NotEqual(t, wait.ApprovalID, again.ApprovalID)
					parked := load(writer, runID)
					require.Equal(t, suspended.ChildRunId, parked.StepStates[parked.FailedStep].ChildRunId)
					require.Equal(t, suspended.Version+1, parked.StepStates[parked.FailedStep].Version, "resumption advances the journal attempt while retaining the same child checkpoint")
					require.Nil(t, parked.Rerun, "a budget decision must not reset execution lineage")
					require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 1}, effects)
					require.Equal(t, 2, reads)
					require.Equal(t, 1, providers[1].calls)
					wait = again
					// Admit the final answer and its evidence review. Both remain charged.
					estimatedCalls = 4
					sendGuidance(secondGuidance)
					require.Equal(t, 1, providers[1].calls)
					fresh := load(reader, runID)
					require.Equal(t, suspended.ChildRunId, fresh.StepStates[fresh.FailedStep].ChildRunId)
					done, err = resume.ResumeFrom(resumeCtx, fresh, triggers[1].template.members["parent"], options)
					expectedReads++
				}
				if rerun {
					var again *workstate.HumanWait
					require.ErrorAs(t, err, &again)
					require.NotEqual(t, wait.ApprovalID, again.ApprovalID)
					parked := load(writer, runID)
					require.Equal(t, suspended.ChildRunId, parked.StepStates[parked.FailedStep].ChildRunId, "same child pauses again; a new invocation must not repeat its prefix")
					require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2}, effects)
					if entry {
						require.Equal(t, 1, reads)
						require.Zero(t, providers[1].calls, "raising calls does not bypass the remaining wall deadline")
					} else {
						require.Equal(t, 2, reads)
						require.Equal(t, 1, providers[1].calls)
					}
					// The first answer cannot authorize this later saved transcript.
					_, oldErr := resume.ResumeFrom(resumeCtx, journal, triggers[1].template.members["parent"], options)
					require.ErrorIs(t, oldErr, automations.ErrNonRetryableStep)
					nextLimit := 8
					if entry {
						nextLimit = 10800000
					}
					if entry {
						estimatedWall = int64(nextLimit)
					} else {
						estimatedCalls = nextLimit
					}
					act("decideApproval", map[string]any{"approvalId": again.ApprovalID, "decision": "answered", "answer": map[string]any{"value": "retry"}})
					require.True(t, peers[0].DispatchRun(person, runID, owner))
					peers[1].DispatchRun(person, runID, owner)
					req := <-admitted
					require.Equal(t, again.ApprovalID, req.HumanResumeId)
					require.Equal(t, journal.Rerun.RequestId, req.RerunRequestId)
					select {
					case <-admitted:
						t.Fatal("second answer dispatched twice")
					default:
					}
					fresh := load(writer, runID)
					freshCtx, contextErr := workExecutionContext(person, fresh, nil, nil)
					require.NoError(t, contextErr)
					fresh, nextOptions, prepErr := automations.PrepareRerun(fresh, nil, triggers[1].template.members["parent"])
					require.NoError(t, prepErr)
					if tail {
						// The exact rerun already completed a read and saved its paired
						// exchange. An expired entry deadline must preserve that full tail.
						estimatedCalls, estimatedWall = 8, 3600000
						_, entryErr := resume.ResumeFrom(freshCtx, fresh, triggers[1].template.members["parent"], nextOptions)
						var entryWait *workstate.HumanWait
						require.ErrorAs(t, entryErr, &entryWait)
						require.NotEqual(t, again.ApprovalID, entryWait.ApprovalID)
						require.Equal(t, 1, providers[1].calls)
						require.Equal(t, 2, reads)
						require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": 2}, effects)
						_, staleErr := resume.ResumeFrom(freshCtx, fresh, triggers[1].template.members["parent"], nextOptions)
						require.ErrorIs(t, staleErr, automations.ErrNonRetryableStep)
						estimatedWall = 10800000
						act("decideApproval", map[string]any{"approvalId": entryWait.ApprovalID, "decision": "answered", "answer": map[string]any{"value": "retry"}})
						require.True(t, peers[0].DispatchRun(person, runID, owner))
						peers[1].DispatchRun(person, runID, owner)
						req := <-admitted
						require.Equal(t, entryWait.ApprovalID, req.HumanResumeId)
						require.Empty(t, admitted, "same answer must not dispatch twice")
						fresh = load(writer, runID)
						freshCtx, contextErr = workExecutionContext(person, fresh, nil, nil)
						require.NoError(t, contextErr)
						fresh, nextOptions, prepErr = automations.PrepareRerun(fresh, nil, triggers[1].template.members["parent"])
						require.NoError(t, prepErr)
					}
					done, err = resume.ResumeFrom(freshCtx, fresh, triggers[1].template.members["parent"], nextOptions)
					if !entry {
						expectedReads++
					}
				}
				require.NoError(t, err)
				require.Equal(t, "completed", done.Status)
				expectedPrefix := 1
				if rerun {
					expectedPrefix = 2
				}
				require.Equal(t, map[string]int{"completed-section": 1, "child-prefix": expectedPrefix, "child-tail": 1, "later-section": 1}, effects)
				require.Equal(t, expectedReads, reads, "the completed read must not replay")
				expectedCalls := 1
				if (rerun && !entry) || boundary == "guidance" {
					expectedCalls = 2
				}
				require.Equal(t, expectedCalls, providers[1].calls)
				raw, err := json.Marshal(providers[1].requests[0])
				require.NoError(t, err)
				require.Contains(t, string(raw), "Recorded answer for this run")
				require.NotEmpty(t, providers[1].requests[0][0].Content, "entry placeholder must be replaced by the installed system prompt")
				if tail {
					last, marshalErr := json.Marshal(providers[1].requests[len(providers[1].requests)-1])
					require.NoError(t, marshalErr)
					require.Contains(t, string(last), "read completed once", "entry checkpoint must retain the prior paired result")
				}
				if boundary == "after-tool" || boundary == "reply-review" {
					require.Contains(t, string(raw), "read completed once")
				}
				if boundary == "guidance" {
					require.Contains(t, string(raw), firstGuidance)
					require.Contains(t, string(raw), "Complete the synthetic section")
					last, marshalErr := json.Marshal(providers[1].requests[1])
					require.NoError(t, marshalErr)
					for _, retained := range []string{firstGuidance, secondGuidance, "Complete the synthetic section", "read completed once"} {
						require.Contains(t, string(last), retained)
					}
					require.Equal(t, 3, providers[0].calls+providers[1].calls, "usage accumulates across both explicit raises")
					var receipts int
					require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:modelCall' AND payload->>'runId' IN (?,?)`, runID, "v1:work:run:"+runID).Scan(person, &receipts))
					require.Equal(t, 1, providers[0].reviews+providers[1].reviews, "the final evidence review must execute once")
					require.Equal(t, 4, receipts, "both replicas persist every answer and its final review across feedback checkpoints")
				}
				var spend string
				require.NoError(t, db.NewRaw(`SELECT payload->'spent' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(person, &spend))
				expectedSpent := expectedCalls
				if boundary == "reply-review" {
					expectedSpent = 2
				}
				if entry {
					expectedSpent = 2
				}
				require.Contains(t, spend, fmt.Sprintf(`"modelCalls": %d`, expectedSpent), "resumption must not reset prior spend")
				freshGuard := work.NewRunCeilings(reader, reader.Logger)
				_, accounted, accountingErr := freshGuard.ModelCallBudget(person, run)
				require.NoError(t, accountingErr)
				totalCalls := providers[0].calls + providers[1].calls + providers[0].reviews + providers[1].reviews
				if entry || boundary == "initial" {
					totalCalls++
				}
				require.Equal(t, totalCalls, accounted, "a fresh replica must account for every turn and final review")

			})
		}
	}
}
