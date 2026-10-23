package work

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/agent"
)

type accountingEmbeddingProbe struct{ calls int }

func (*accountingEmbeddingProbe) IntegrationName() string { return "embedding" }
func (p *accountingEmbeddingProbe) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{Name: "store", Handler: func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
		p.calls++
		return nil, fmt.Errorf("accounting must not request recursive provider admission")
	}}}
}

func TestAccountingReceiptsPersistWithoutRecursiveEmbedding(t *testing.T) {
	a := openActsDB(t)
	require.NoError(t, a.eng.RegisterIntegration(a.i))
	runID := a.openRun(t, nil)
	rc := common.RunContext{RunId: runID, GoalId: rowString(a.runRow(t, runID), "goalId"), OwnerUserId: a.owner, StepKey: "research", Mode: common.RunModeLive}
	a.write(t, "updateWorkGoal", map[string]any{"goalId": rc.GoalId, "ceilings": map[string]any{"maxModelCalls": 96}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "classification": map[string]any{"workload": "lookup"}})
	ctx := common.ContextWithRun(actorCtx(a.owner), rc)
	journal := NewModelJournal(a.eng)
	for n := 0; n < 12; n++ {
		require.NoError(t, journal.Record(ctx, a.owner, memql.JournaledCall{ReceiptID: fmt.Sprintf("%s-call-%d", memql.BareShortId(runID), n), RunId: runID, Served: memql.ServedLocal}))
	}
	require.NoError(t, a.i.RecordToolInvocation(ctx, agent.ToolInvocation{RunId: runID, OwnerUserId: a.owner, ToolName: "readSource", NativeResultEvidence: workstate.ArtifactHash(map[string]any{"finding": "new source evidence"})}))
	probe := &accountingEmbeddingProbe{}
	require.NoError(t, a.eng.RegisterIntegration(probe))
	guard := NewRunCeilings(a.eng, testLogger())
	a.eng.SetRunCeilingGuard(guard)
	require.Nil(t, guard.Admit(ctx, rc, 0))
	require.Equal(t, 1, rowInt(rowMap(rowMap(a.runRow(t, runID), "classification"), "progressContinuation"), "extensions"))
	require.NoError(t, guard.AdmitProvider(ctx, rc, memql.ProviderAdmission{}))
	rows := a.query(t, a.owner, "query "+call("workProviderAdmissionForOwnerRun", map[string]any{"runId": runID}))
	require.Len(t, rows, 1)
	require.Equal(t, 1, rowInt(rowMap(rowMap(rows[0], "data"), "providerAdmission"), "admittedCalls"))
	require.Zero(t, probe.calls, "renewal and provider receipts remain durable without dispatching optional embeddings")
}
