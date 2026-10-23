package work

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agent"
)

func TestProviderAdmissionsReserveAcrossReplicasAndKeepHardLimits(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
	}
	runID := a.openRun(t, nil)
	rc := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "compare-records", Mode: common.RunModeLive}
	guards := []*RunCeilings{NewRunCeilings(a.eng, testLogger()), NewRunCeilings(b.eng, testLogger())}
	// A faster peer clock cannot cause the next writer's cumulative receipt to
	// disappear behind the previous head. The gate fences its timestamp too.
	fastClock := time.Now().Add(time.Minute)
	a.i.SetNow(func() time.Time { return fastClock })
	results := make([]error, 6)
	var wg sync.WaitGroup
	for n := range results {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			results[n] = guards[n%2].AdmitProvider(actorCtx(a.owner), rc, memql.ProviderAdmission{HardCallLimit: 3})
		}(n)
	}
	wg.Wait()
	accepted := 0
	for _, err := range results {
		if err == nil {
			accepted++
			continue
		}
		var refusal *memql.RunCeilingError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, "providerCalls", refusal.Breach.Ceiling)
	}
	require.Equal(t, 3, accepted)
	fresh := NewRunCeilings(b.eng, testLogger())
	require.Error(t, fresh.AdmitProvider(actorCtx(a.owner), rc, memql.ProviderAdmission{HardCallLimit: 3}), "restart cannot reset admitted usage")
	rows := b.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID}))
	require.Len(t, rows, 1)
	require.Equal(t, 3, rowInt(rowMap(rowMap(rows[0], "data"), "providerAdmission"), "admittedCalls"))
	require.Empty(t, b.query(t, "another-owner", "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID})))
	// Monetary ceilings include reserved auxiliary calls, even before any text
	// answer has produced an ordinary modelCall usage row.
	costRun := a.openRun(t, nil)
	costRC := rc
	costRC.RunId = costRun
	costRC.GoalId = rowString(a.runRow(t, costRun), "goalId")
	a.write(t, "updateWorkGoal", map[string]any{"goalId": costRC.GoalId, "ceilings": map[string]any{"costCeiling": 0.3}})
	require.NoError(t, guards[0].AdmitProvider(actorCtx(a.owner), costRC, memql.ProviderAdmission{EstimatedCost: 0.2}))
	err := guards[1].AdmitProvider(actorCtx(a.owner), costRC, memql.ProviderAdmission{EstimatedCost: 0.2})
	var costBreach *memql.RunCeilingError
	require.ErrorAs(t, err, &costBreach)
	require.Equal(t, "cost", costBreach.Breach.Ceiling)
	tokenRun := a.openRun(t, nil)
	tokenRC := rc
	tokenRC.RunId = tokenRun
	tokenRC.GoalId = rowString(a.runRow(t, tokenRun), "goalId")
	a.write(t, "updateWorkGoal", map[string]any{"goalId": tokenRC.GoalId, "ceilings": map[string]any{"tokenBudget": 100}})
	require.NoError(t, guards[0].AdmitProvider(actorCtx(a.owner), tokenRC, memql.ProviderAdmission{EstimatedTokens: 60}))
	err = guards[1].AdmitProvider(actorCtx(a.owner), tokenRC, memql.ProviderAdmission{EstimatedTokens: 60})
	require.ErrorAs(t, err, &costBreach)
	require.Equal(t, "tokens", costBreach.Breach.Ceiling)
	// A legacy run cannot certify an explicit hard cap from logical answer
	// counts alone. Its unobserved transport history remains visibly unknown.
	legacy := a.openRun(t, nil)
	legacyRC := rc
	legacyRC.RunId = legacy
	legacyRC.GoalId = rowString(a.runRow(t, legacy), "goalId")
	_, err = a.db.ExecContext(context.Background(), `UPDATE "MemoryNodes" SET payload=payload-'providerAccountingVersion' WHERE id=?`, legacy)
	require.NoError(t, err)
	require.ErrorContains(t, guards[0].AdmitProvider(actorCtx(a.owner), legacyRC, memql.ProviderAdmission{HardCallLimit: 600}), "historical provider attempts are unknown")
	require.NoError(t, guards[0].AdmitProvider(actorCtx(a.owner), legacyRC, memql.ProviderAdmission{}))
	legacyRows := b.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": legacy}))
	require.Equal(t, "legacy-attempts-unknown", rowString(rowMap(rowMap(legacyRows[0], "data"), "providerAdmission"), "coverage"))
	// Malformed heads cannot look like a fresh, unused ledger.
	require.NoError(t, a.i.store().writeInternal(memql.ContextWithTrainingEvidenceWrite(actorCtx(a.owner)), "mutation "+call("createWorkObservation", map[string]any{"observationId": newRowId("v1:work:observation"), "runId": runID, "kind": "decision", "versionTime": rfc(fastClock.Add(time.Minute)), "content": "malformed fixture", "data": map[string]any{"providerAdmission": map[string]any{"schemaVersion": 2}}})))
	err = fresh.AdmitProvider(actorCtx(a.owner), rc, memql.ProviderAdmission{})
	require.ErrorContains(t, err, "malformed or has an unsupported version")
}

func TestProviderAdmissionDoesNotInvertConcurrentProgressLocks(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
	}
	runID := a.openRun(t, nil)
	rc := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "research", Mode: common.RunModeLive}
	a.write(t, "updateWorkGoal", map[string]any{"goalId": rc.GoalId, "ceilings": map[string]any{"maxModelCalls": 96}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}})
	ctx, cancel := context.WithTimeout(actorCtx(a.owner), 15*time.Second)
	defer cancel()
	providerLocked, logicalWaiting, allowProvider := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	gate := a.i.decisionGate
	a.i.decisionGate = func(ctx context.Context, keys ...string) (func(), error) {
		if len(keys) == 1 {
			select {
			case logicalWaiting <- struct{}{}:
			default:
			}
			return gate(ctx, keys...)
		}
		release, err := gate(ctx, keys...)
		if err != nil {
			return nil, err
		}
		close(providerLocked)
		select {
		case <-allowProvider:
			return release, nil
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		}
	}
	guard := NewRunCeilings(a.eng, testLogger())
	providerResult := make(chan error, 1)
	go func() { providerResult <- guard.AdmitProvider(ctx, rc, memql.ProviderAdmission{}) }()
	select {
	case <-providerLocked:
	case <-ctx.Done():
		t.Fatal("provider did not reach its reservation gate")
	}
	// Peer completions can become visible after the provider's initial logical
	// check. The concurrent admission now needs the work gate to renew.
	journal := NewModelJournal(b.eng)
	for n := 0; n < 12; n++ {
		require.NoError(t, journal.Record(ctx, a.owner, memql.JournaledCall{ReceiptID: fmt.Sprintf("%s-peer-%d", memql.BareShortId(runID), n), RunId: runID, Served: memql.ServedLocal}))
	}
	require.NoError(t, b.i.RecordToolInvocation(ctx, agent.ToolInvocation{RunId: runID, OwnerUserId: a.owner, ToolName: "readSource", NativeResultEvidence: workstate.ArtifactHash(map[string]any{"source": "new substantive source"}), Args: map[string]any{}}))
	logicalResult := make(chan *memql.RunCeilingBreach, 1)
	go func() { logicalResult <- guard.Admit(ctx, rc, 0) }()
	select {
	case <-logicalWaiting:
	case <-ctx.Done():
		t.Fatal("logical admission did not reach progress renewal")
	}
	close(allowProvider)
	require.NoError(t, <-providerResult, "provider must not wait for the logical budget mutex while holding the work gate")
	require.Nil(t, <-logicalResult)
}
