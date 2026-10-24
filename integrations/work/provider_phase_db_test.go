package work

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type providerCompileResult struct {
	ctx context.Context
	run common.RunContext
	err error
}

type providerCompileProbe struct {
	guard           *RunCeilings
	results         chan providerCompileResult
	release         chan struct{}
	beforeAdmission func()
}

func (p *providerCompileProbe) Compile(ctx context.Context, _ CompileRequest) {
	rc, _ := common.RunFromContext(ctx)
	if p.beforeAdmission != nil {
		p.beforeAdmission()
	}
	p.results <- providerCompileResult{ctx: ctx, run: rc, err: p.guard.AdmitProvider(ctx, rc, memql.ProviderAdmission{HardCallLimit: 3})}
	<-p.release
}

// Intake has no local compiler or provider state. A different replica claims
// the stored run, reconstructs its authority, and reaches the real durable
// provider-admission boundary while the run is still compiling.
func TestProviderAdmissionCompileCrossesReplicaBeforeExecution(t *testing.T) {
	t.Setenv("MEMQL_NODE_ID", "provider-compile-receiver")
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	_, err := a.db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload) VALUES (?, 'v1:identity:user', now(), 'provider-phase-test', '{}', '{"role":"writer","active":true}')`, a.owner)
	require.NoError(t, err)
	require.NoError(t, b.eng.RegisterIntegration(b.i))
	a.i.EnableCompileViaEvent()
	probe := &providerCompileProbe{guard: NewRunCeilings(b.eng, testLogger()), results: make(chan providerCompileResult, 1), release: make(chan struct{})}
	b.i.SetCompiler(probe)
	defer close(probe.release)
	nodes, err := a.i.handleCreateGoal(actorCtx(a.owner), map[string]any{"statement": "Explain the saved report", "ceilings": map[string]any{"maxModelCalls": 12}}, 0)
	require.NoError(t, err)
	reply := decodeReply(t, nodes)
	runID := reply["runId"].(string)
	b.i.HandleRunEvent(runEvent(runID, runStatusCompiling, "work.compile", a.owner))
	var got providerCompileResult
	select {
	case got = <-probe.results:
	case <-time.After(5 * time.Second):
		t.Fatal("receiving planner did not reach provider admission")
	}
	require.NoError(t, got.err, "a claimed compiler must be able to make its first inference")
	require.Equal(t, runStatusCompiling, rowString(a.runRow(t, runID), "status"))
	rows := a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID}))
	require.Len(t, rows, 1)
	require.Equal(t, 1, rowInt(rowMap(rowMap(rows[0], "data"), "providerAdmission"), "admittedCalls"))
	// Owning the run is not permission to execute an unclaimed compiler, and
	// a stopped compiler's context must not become ordinary execution authority.
	require.Error(t, probe.guard.AdmitProvider(actorCtx(a.owner), got.run, memql.ProviderAdmission{}))
	otherGoal := got.run
	otherGoal.GoalId = newRowId(goalConcept)
	require.Error(t, probe.guard.AdmitProvider(got.ctx, otherGoal, memql.ProviderAdmission{}))
	otherOwner := got.run
	otherOwner.OwnerUserId = newRowId("v1:identity:user")
	require.Error(t, probe.guard.AdmitProvider(got.ctx, otherOwner, memql.ProviderAdmission{}))
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "nodeId": "another-compiler"})
	require.Error(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{}))
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "nodeId": "provider-compile-receiver"})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true})
	require.Error(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{}))
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "cancelRequested": false, "status": runStatusRunning})
	require.Error(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{}))
	for _, status := range []string{runStatusFailed, runStatusSucceeded, runStatusCancelled, runStatusAbandoned} {
		a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": status})
		require.Error(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{}), status)
		require.Error(t, probe.guard.AdmitProvider(actorCtx(a.owner), got.run, memql.ProviderAdmission{}), status)
	}
	rows = a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID}))
	require.Equal(t, 1, rowInt(rowMap(rowMap(rows[0], "data"), "providerAdmission"), "admittedCalls"), "refused phases must not reserve inference")
}

type providerRemedyProbe struct{ *providerCompileProbe }

func (p providerRemedyProbe) Replan(ctx context.Context, _, _, _, _ string) bool {
	p.Compile(ctx, CompileRequest{})
	return true
}

func (p providerRemedyProbe) Repair(ctx context.Context, _, _, _, _ string) bool {
	p.Compile(ctx, CompileRequest{})
	return true
}

func TestProviderAdmissionRemedyIsBoundToClaimedWaitAcrossReplicas(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	_, err := a.db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload) VALUES (?, 'v1:identity:user', now(), 'provider-phase-test', '{}', '{"role":"writer","active":true}')`, a.owner)
	require.NoError(t, err)
	require.NoError(t, b.eng.RegisterIntegration(b.i))
	b.i.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return b.db }, testLogger()).StrictClaimer())
	for _, kind := range []string{waitKindReplan, waitKindRepair} {
		t.Run(kind, func(t *testing.T) {
			runID := a.openRun(t, nil)
			since := rfc(time.Now())
			wait := map[string]any{"kind": kind, "subject": "draft", "since": since, "reason": "repair the draft"}
			a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusWaiting, "waitingOn": wait})
			probe := &providerCompileProbe{guard: NewRunCeilings(b.eng, testLogger()), results: make(chan providerCompileResult, 1), release: make(chan struct{})}
			defer close(probe.release)
			b.i.SetRemedy(providerRemedyProbe{probe})
			require.True(t, b.i.handOffRemedy(context.Background(), kind, runID, a.owner, since))
			var got providerCompileResult
			select {
			case got = <-probe.results:
			case <-time.After(5 * time.Second):
				t.Fatal("receiving planner did not reach remedy provider admission")
			}
			require.NoError(t, got.err)
			require.Error(t, probe.guard.AdmitProvider(actorCtx(a.owner), got.run, memql.ProviderAdmission{}), "a waiting owner context alone is not a remedy claim")
			require.NoError(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{HardCallLimit: 2}))
			require.Error(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{HardCallLimit: 2}), "phase admission must retain the durable cumulative cap")
			wait["since"] = rfc(time.Now().Add(time.Minute))
			a.write(t, "updateWorkRun", map[string]any{"runId": runID, "waitingOn": wait})
			require.Error(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{}), "a later wait cannot inherit the prior remedy claim")
			wait["since"], wait["kind"] = since, "approval"
			a.write(t, "updateWorkRun", map[string]any{"runId": runID, "waitingOn": wait})
			require.Error(t, probe.guard.AdmitProvider(got.ctx, got.run, memql.ProviderAdmission{}), "human approval waits cannot infer")
			rows := a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID}))
			require.Equal(t, 2, rowInt(rowMap(rowMap(rows[0], "data"), "providerAdmission"), "admittedCalls"))
		})
	}
}

type providerDelayedClaimer struct {
	RunClaimer
	afterClaim func()
}

func (c providerDelayedClaimer) ClaimWithTTL(ctx context.Context, name, key string, ttl time.Duration) bool {
	ok := c.RunClaimer.ClaimWithTTL(ctx, name, key, ttl)
	if ok {
		c.afterClaim()
	}
	return ok
}

func TestProviderAdmissionRemedyClaimDelayConsumesOriginalWindow(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	_, err := a.db.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload) VALUES (?, 'v1:identity:user', now(), 'provider-phase-test', '{}', '{"role":"writer","active":true}')`, a.owner)
	require.NoError(t, err)
	require.NoError(t, b.eng.RegisterIntegration(b.i))
	clock := time.Now()
	b.i.SetNow(func() time.Time { return clock })
	b.i.SetRunClaimer(providerDelayedClaimer{
		RunClaimer: automations.NewClusterExecutionGuard(func() *bun.DB { return b.db }, testLogger()).StrictClaimer(),
		afterClaim: func() { clock = clock.Add(8 * time.Minute) },
	})
	runID := a.openRun(t, nil)
	since := rfc(clock)
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusWaiting, "waitingOn": map[string]any{"kind": waitKindReplan, "subject": "draft", "since": since, "reason": "repair the draft"}})
	probe := &providerCompileProbe{guard: NewRunCeilings(b.eng, testLogger()), results: make(chan providerCompileResult, 1), release: make(chan struct{}), beforeAdmission: func() { clock = clock.Add(3 * time.Minute) }}
	defer close(probe.release)
	b.i.SetRemedy(providerRemedyProbe{probe})
	require.True(t, b.i.handOffRemedy(context.Background(), waitKindReplan, runID, a.owner, since))
	select {
	case got := <-probe.results:
		require.ErrorContains(t, got.err, "admitted work phase expired")
		_, bounded := got.ctx.Deadline()
		require.True(t, bounded)
	case <-time.After(5 * time.Second):
		t.Fatal("receiving planner did not reach remedy provider admission")
	}
	require.Empty(t, a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID})))
}

type providerJudgeProbe struct {
	*memql.MemQLEngine
	guard        *RunCeilings
	result       providerCompileResult
	beforeRender func()
}

func (p *providerJudgeProbe) RenderPrompt(name string, data map[string]any) (string, error) {
	if p.beforeRender != nil {
		p.beforeRender()
	}
	return p.MemQLEngine.RenderPrompt(name, data)
}

func (p *providerJudgeProbe) CallAIStructured(ctx context.Context, _ airoute.ResolveRequest, _ []common.ChatMessage, _ common.StructuredSchema) (memql.StructuredAIResult, error) {
	rc, _ := common.RunFromContext(ctx)
	err := p.guard.AdmitProvider(ctx, rc, memql.ProviderAdmission{HardCallLimit: 2})
	p.result = providerCompileResult{ctx: ctx, run: rc, err: err}
	return memql.StructuredAIResult{Text: `{"product":true,"process":true,"performance":true,"reason":"The saved report answers the request."}`}, err
}

func TestProviderAdmissionAnswerCheckBindsSucceededVersionAcrossReplicas(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	probe := &providerJudgeProbe{MemQLEngine: b.eng}
	b.i.engine = probe
	require.NoError(t, b.eng.RegisterIntegration(b.i))
	probe.guard = NewRunCeilings(probe, testLogger())
	b.i.ServeAnswerChecks(automations.NewClusterExecutionGuard(func() *bun.DB { return b.db }, testLogger()).StrictClaimer())
	runID := a.openRun(t, nil)
	a.writeVersion(t, runID, runID, "draft", 0, 1, nil, "Saved report: three requests were completed.", nil)
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 1}})
	a.finishRun(t, runID, []string{"draft"}, nil)
	result, err := b.i.validateAnswer(actorCtx(a.owner), a.owner, runID)
	require.NoError(t, err)
	require.NotEmpty(t, result["observationId"], "the actual native validator must reach provider admission")
	require.NoError(t, probe.result.err)
	require.Error(t, probe.guard.AdmitProvider(actorCtx(a.owner), probe.result.run, memql.ProviderAdmission{}), "succeeded alone does not authorize a model call")
	require.Error(t, probe.guard.AdmitProvider(context.WithoutCancel(probe.result.ctx), probe.result.run, memql.ProviderAdmission{}), "the committed validation invalidates the old version's scope")
	rows := a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID}))
	require.Equal(t, 1, rowInt(rowMap(rowMap(rows[0], "data"), "providerAdmission"), "admittedCalls"))
}

func TestProviderAdmissionAnswerCheckCannotExtendClaimDuringPromptPreparation(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	clock := time.Now()
	b.i.SetNow(func() time.Time { return clock })
	probe := &providerJudgeProbe{MemQLEngine: b.eng, beforeRender: func() { clock = clock.Add(validatorClaimTTL + time.Second) }}
	b.i.engine = probe
	require.NoError(t, b.eng.RegisterIntegration(b.i))
	probe.guard = NewRunCeilings(probe, testLogger())
	b.i.ServeAnswerChecks(automations.NewClusterExecutionGuard(func() *bun.DB { return b.db }, testLogger()).StrictClaimer())
	runID := a.openRun(t, nil)
	a.writeVersion(t, runID, runID, "draft", 0, 1, nil, "Saved report: three requests were completed.", nil)
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 1}})
	a.finishRun(t, runID, []string{"draft"}, nil)
	result, err := b.i.validateAnswer(actorCtx(a.owner), a.owner, runID)
	require.NoError(t, err)
	require.ErrorContains(t, probe.result.err, "admitted work phase expired")
	require.Contains(t, rowString(result, "skipped"), "admitted work phase expired")
	_, bounded := probe.result.ctx.Deadline()
	require.True(t, bounded, "the in-flight call must also end inside the claim window")
	require.Empty(t, a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID})))
}
