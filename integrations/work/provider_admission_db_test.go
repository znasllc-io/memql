package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
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
