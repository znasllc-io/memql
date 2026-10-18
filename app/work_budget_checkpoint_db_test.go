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
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/agents"
	work "github.com/znasllc-io/memql/integrations/work"
)

// Only the model/tool boundary is controlled. The installed runAgentTurn,
// checkpoint proof, nested executor journals, decision and dispatch are real.
type budgetTurnFixture struct {
	engine       *memql.MemQLEngine
	effects      map[string]int
	paused       func(string)
	cancelled    bool
	repeatBudget bool
	beforeBudget func(context.Context)
}

func (f *budgetTurnFixture) ResolveOwnerAgent(context.Context, string) (string, error) {
	return "fixture-agent", nil
}
func (f *budgetTurnFixture) RunTurn(ctx context.Context, _ *memqlv1.AgentGenerateTurnMsg) (string, error) {
	rc, _ := common.RunFromContext(ctx)
	if rc.Continuation != nil {
		messages, err := f.engine.RestoreWorkContinuation(ctx, nil)
		if err != nil {
			return "", err
		}
		if (len(messages) != 3 && len(messages) != 4) || messages[2].Content != "effect already performed" {
			return "", fmt.Errorf("completed tool receipt lost")
		}
		if !rc.Override.Empty() || rc.Mode == common.RunModeReplay {
			return "", fmt.Errorf("continuation expanded permissions")
		}
		if f.repeatBudget {
			f.repeatBudget = false
			messages = append(messages, common.ChatMessage{Role: "assistant", Content: "Reached another reasoning boundary"})
			return "", f.engine.PauseWorkForBudget(ctx, &memql.RunCeilingError{RunId: rc.RunId, StepKey: rc.StepKey,
				Breach: memql.RunCeilingBreach{Ceiling: "wallClock", Limit: "3600000ms", Actual: "3600001ms", Reason: "the run reached its wall-clock ceiling"},
				Spent:  memql.RunSpend{ModelCalls: 4, TokensLocal: 700, WallClockMs: 3600001}}, messages)
		}
		f.effects["continued"]++
		return "continued from completed tool", nil
	}
	f.effects["tool"]++
	messages := []common.ChatMessage{{Role: "user", Content: "Synthetic bounded work"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "saved", Name: "effect", Arguments: `{}`}}}, {Role: "tool", ToolCallId: "saved", Name: "effect", Content: "effect already performed"}}
	ceiling := &memql.RunCeilingError{RunId: rc.RunId, StepKey: rc.StepKey, Breach: memql.RunCeilingBreach{Ceiling: "wallClock", Limit: "600000ms", Actual: "600001ms", Reason: "the run reached its wall-clock ceiling"}, Spent: memql.RunSpend{ModelCalls: 3, TokensLocal: 500, WallClockMs: 600001}}
	if f.cancelled {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		cancel()
	}
	if f.beforeBudget != nil {
		f.beforeBudget(ctx)
	}
	err := f.engine.PauseWorkForBudget(ctx, ceiling, messages)
	var wait *workstate.HumanWait
	if errors.As(err, &wait) && f.paused != nil {
		f.paused(wait.ApprovalID)
	}
	return "", err
}

func TestWorkDBBudgetCheckpointResumesNestedAgentOnAnotherReplica(t *testing.T) {
	for _, mode := range []string{"ordinary", "early-answer", "repeated", "checkpoint-write-failure", "coordination-failure", "cancelled", "closed-goal"} {
		t.Run(mode, func(t *testing.T) {
			early := mode == "early-answer"
			refused := mode == "checkpoint-write-failure" || mode == "coordination-failure" || mode == "cancelled" || mode == "closed-goal"
			writer, db := workTemplateDBEngineAndDB(t)
			reader := workTemplateDBEngine(t)
			gates, err := database.NewGatePool(dbtest.DSN(), 0)
			require.NoError(t, err)
			defer gates.Close()
			key := strings.ReplaceAll(id.NewShortId(), "-", "")
			owner, runID, goalID := key+"owner", key+"run", key+"goal"
			person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
			ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive})
			effects := map[string]int{}
			runners := []*budgetTurnFixture{{engine: writer, effects: effects, cancelled: true}, {engine: reader, effects: effects, repeatBudget: mode == "repeated"}}
			peers := []*work.Integration{}
			triggers := []*workTemplateTrigger{}
			for n, e := range []*memql.MemQLEngine{writer, reader} {
				var wi *work.Integration
				for _, plugin := range memql.RegisteredPlugins() {
					if plugin.Name != "work" {
						continue
					}
					provider, err := plugin.Factory(memql.PluginContext{Engine: e, Logger: e.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, AdmitSourceRow: memql.AdmitSourceRow})
					require.NoError(t, err)
					wi = provider.(*work.Integration)
				}
				require.NotNil(t, wi)
				require.NoError(t, e.RegisterIntegration(wi))
				peers = append(peers, wi)
				ai := agents.New(e.Agents(), e)
				ai.SetAgentTurnRunner(runners[n])
				require.NoError(t, e.RegisterIntegration(ai))
				fixture := &childPauseFixture{engine: e, t: t, effects: effects}
				require.NoError(t, e.RegisterIntegration(fixture))
				require.NoError(t, e.Functions().Upsert(&memql.Function{Name: "fixtureeffect", Type: memql.FunctionTypeBuiltin, Enabled: true, BuiltinArgs: &memql.BuiltinArgContract{Profile: memql.BuiltinArgProfileOptionalObject, Properties: map[string]string{"label": "string"}}, ArgsSchema: &memql.ArgsSchemaConfig{Fields: []*memql.FunctionArgsField{{Name: "label", Type: "string"}}}, FunctionKind: "builtin", Executor: "integration.childPauseFixture.effect"}))
				loader := automations.NewLoader(automations.LoaderOptions{Functions: e.Functions()})
				tpl := &workTemplate{members: map[string]*automations.Automation{}}
				for _, src := range []string{
					`@template automation leaf { builtin fixtureeffect(label: "leaf"); answer := builtin runAgentTurn(prompt: "Synthetic budget continuation") retry(2) on error continue; builtin fixtureeffect(label: "leaf-tail"); return answer }`,
					`@template automation middle { builtin fixtureeffect(label: "middle"); answer := automation leaf(); return answer }`,
					`@template automation parent { builtin fixtureeffect(label: "parent"); answer := automation middle(); builtin fixtureeffect(label: "parent-tail"); return answer }`,
				} {
					a, err := loader.CompileSource(strings.ReplaceAll(src, "; ", "\n"), "budget-fixture.memql")
					require.NoError(t, err)
					tpl.members[a.Name] = a
				}
				triggers = append(triggers, &workTemplateTrigger{app: &App{engine: e, stepRegistry: steps.NewRegistry(), automationLoader: loader, Logger: e.Logger}, template: tpl})
			}
			write := func(name string, args map[string]any) {
				templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
			}
			write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic budget checkpoint", "origin": "user", "ceilings": map[string]any{"wallClockMs": 7200000, "maxModelCalls": 96, "maxRetries": 3}})
			write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": triggers[0].template.members["parent"].DefinitionFingerprint(id.New()), "mode": "live", "status": "running", "startedAt": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano), "spent": map[string]any{"retries": 2, "events": 7}})
			write("updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}, "spent": map[string]any{"retries": 2, "events": 7}})
			if mode == "checkpoint-write-failure" {
				name := "budgetfail" + strings.ToLower(key)
				_, err := db.ExecContext(person, `CREATE FUNCTION `+name+`() RETURNS trigger AS $$ BEGIN IF NEW.concept='v1:work:observation' AND NEW.payload->>'runId'='v1:work:run:`+runID+`' AND NEW.payload->'data'->>'continuation'='true' THEN RAISE EXCEPTION 'synthetic checkpoint write failure'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql`)
				require.NoError(t, err)
				_, err = db.ExecContext(person, `CREATE TRIGGER `+name+` BEFORE INSERT ON "MemoryNodes" FOR EACH ROW EXECUTE FUNCTION `+name+`()`)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, _ = db.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS `+name+` ON "MemoryNodes"`)
					_, _ = db.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS `+name+`()`)
				})
			}
			if mode == "coordination-failure" {
				peers[0].SetGateDB(nil)
			}
			if mode == "cancelled" {
				runners[0].beforeBudget = func(context.Context) { write("updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true}) }
			}
			if mode == "closed-goal" {
				runners[0].beforeBudget = func(context.Context) { write("updateWorkGoal", map[string]any{"goalId": goalID, "status": "closed"}) }
			}
			var approval string
			decide := func() {
				for _, cap := range peers[1].Capabilities() {
					if cap.Name == "decideApproval" {
						_, err := cap.Handler(person, map[string]any{"approvalId": approval, "decision": "answered", "answer": map[string]any{"value": "retry"}}, 0)
						require.NoError(t, err)
						return
					}
				}
				t.Fatal("missing actual decision capability")
			}
			if early {
				runners[0].paused = func(id string) {
					approval = id
					decide()
					d := &workRunDispatcher{app: &App{engine: reader, Logger: reader.Logger}}
					_, ready := d.PrepareDispatch(person, work.DispatchRequest{RunId: runID, HumanResumeId: approval})
					require.False(t, ready, "answer cannot precede durable unwind")
				}
			}
			t.Setenv("MEMQL_NODE_ID", "agent-origin")
			ex := triggers[0].executor()
			defer ex.Close()
			execution, err := ex.ExecuteAdopted(ctx, triggers[0].template.members["parent"], automations.RunAdoption{RunId: runID})
			if refused {
				require.ErrorIs(t, err, automations.ErrJournalRequired)
				require.Equal(t, map[string]int{"parent": 1, "middle": 1, "leaf": 1, "tool": 1}, effects, "failed suspension must ignore authored continue/retry")
				var approvals int
				require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:approval' AND payload->>'ownerUserId' IN (?,?)`, owner, "v1:identity:user:"+owner).Scan(person, &approvals))
				require.Zero(t, approvals)
				return
			}
			var wait *workstate.HumanWait
			require.ErrorAs(t, err, &wait)
			require.Equal(t, "waiting", execution.Status)
			approval = wait.ApprovalID
			load := func(e *memql.MemQLEngine, id string) *automations.RunJournal {
				j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), e, id)
				require.NoError(t, err)
				return j
			}
			journal := load(reader, runID)
			middle := load(reader, journal.StepStates[journal.FailedStep].ChildRunId)
			leaf := load(reader, middle.StepStates[middle.FailedStep].ChildRunId)
			for _, child := range []*automations.RunJournal{middle, leaf} {
				require.Equal(t, "waiting", child.Status)
				require.Equal(t, approval, memql.BareShortId(fmt.Sprint(child.WaitingOn["subject"])))
			}
			require.Equal(t, "waiting", journal.StepStates[journal.FailedStep].Status)
			require.Zero(t, effects["leaf-tail"])
			require.Zero(t, effects["parent-tail"])
			var count int
			require.NoError(t, db.NewRaw(`SELECT count(DISTINCT id) FROM "MemoryNodes" WHERE concept='v1:work:approval' AND payload->>'ownerUserId' IN (?,?)`, owner, "v1:identity:user:"+owner).Scan(person, &count))
			require.Equal(t, 1, count, "nested unwind must not mint another question")
			if !early {
				decide()
			}
			journal = load(reader, runID)
			require.Equal(t, "running", journal.Status)
			require.Nil(t, journal.Rerun)
			// Real readiness + shared claims admit one answer, even while original lease held.
			admitted := make(chan work.DispatchRequest, 3)
			for n, peer := range peers {
				peer.SetDispatcher(&humanReadyDispatcher{workRunDispatcher: &workRunDispatcher{app: triggers[n].app}, admitted: admitted})
				peer.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer())
			}
			require.True(t, automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer().ClaimWithTTL(person, "work.run.dispatch", runID, 4*time.Minute))
			require.True(t, peers[0].DispatchRun(person, runID, owner))
			peers[1].DispatchRun(person, runID, owner)
			select {
			case req := <-admitted:
				require.Equal(t, approval, req.HumanResumeId)
			case <-time.After(3 * time.Second):
				t.Fatal("budget answer not dispatched")
			}
			select {
			case <-admitted:
				t.Fatal("duplicate answer executed")
			default:
			}
			// An approved legacy budget with no checkpoint binding, or a changed
			// immutable receipt, cannot authorize replay of arbitrary failed work.
			var approvalJSON string
			approvalID := "v1:work:approval:" + approval
			require.NoError(t, db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, approvalID).Scan(person, &approvalJSON))
			var original map[string]any
			require.NoError(t, json.Unmarshal([]byte(approvalJSON), &original))
			for _, negative := range []string{"legacy", "changed checkpoint", "unapproved"} {
				var changed map[string]any
				require.NoError(t, json.Unmarshal([]byte(approvalJSON), &changed))
				subject := changed["subject"].(map[string]any)
				switch negative {
				case "legacy":
					delete(subject, "resumeKind")
				case "changed checkpoint":
					subject["continuationId"] = "another-receipt"
				case "unapproved":
					delete(changed, "decision")
				}
				insertWorkRow(t, db, approvalID, "v1:work:approval", time.Now().UTC(), changed)
				denied := triggers[1].executor()
				_, err := denied.ResumeFrom(ctx, journal, triggers[1].template.members["parent"], nil)
				denied.Close()
				require.Error(t, err, negative)
				require.Zero(t, effects["continued"])
				insertWorkRow(t, db, approvalID, "v1:work:approval", time.Now().UTC(), original)
			}
			t.Setenv("MEMQL_NODE_ID", "agent-resumer")
			resume := triggers[1].executor()
			defer resume.Close()
			done, err := resume.ResumeFrom(ctx, journal, triggers[1].template.members["parent"], nil)
			if mode == "repeated" {
				var secondWait *workstate.HumanWait
				require.ErrorAs(t, err, &secondWait)
				require.NotEqual(t, approval, secondWait.ApprovalID)
				oldApproval := approval
				approval = secondWait.ApprovalID
				decide()
				j := load(writer, runID)
				require.Equal(t, approval, j.HumanResumeId)
				old := *j
				old.HumanResumeId = oldApproval
				again := triggers[0].executor()
				defer again.Close()
				_, err = again.ResumeFrom(ctx, &old, triggers[0].template.members["parent"], nil)
				require.Error(t, err, "old budget receipt cannot resume a changed checkpoint")
				done, err = again.ResumeFrom(ctx, j, triggers[0].template.members["parent"], nil)
			}
			require.NoError(t, err)
			require.Equal(t, "completed", done.Status)
			require.Equal(t, map[string]int{"parent": 1, "middle": 1, "leaf": 1, "tool": 1, "continued": 1, "leaf-tail": 1, "parent-tail": 1}, effects)
			var resumedNode string
			require.NoError(t, db.NewRaw(`SELECT payload->>'nodeId' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(person, &resumedNode))
			require.Equal(t, "agent-resumer", resumedNode, "resume must name the replica doing the work")
			var raw string
			require.NoError(t, db.NewRaw(`SELECT payload->'spent' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(person, &raw))
			require.Contains(t, raw, `"retries": 2`)
			if mode == "repeated" {
				require.Contains(t, raw, `"modelCalls": 4`)
			} else {
				require.Contains(t, raw, `"modelCalls": 3`)
			}
		})
	}
}
