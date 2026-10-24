//go:build agent

package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	permission "github.com/znasllc-io/memql/core/computerpermission"
	"github.com/znasllc-io/memql/core/id"
	work "github.com/znasllc-io/memql/integrations/work"
)

// An approval can arrive more than two seconds before an App finishes
// unwinding. The cron leader may be an identity/BFF node with no dispatcher:
// it cannot supply the agent-only sweep that the older regression simulated.
// The settled executor must re-offer the same answer, with no repeated effect
// or new execution generation, before that unrelated sweep judges liveness.
func TestWorkDBHumanSettlementWakesWithoutExecutingSweepLeader(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	key := id.NewShortId()
	owner, runID, goalID, approval, child := key+"owner", key+"run", key+"goal", key+"approval", key+"child"
	rerun := key + "rerun"
	ctx := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(ctx), name, args)
	}
	now := time.Now().UTC()
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic early answer with non-executing sweep leader", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "parent", "templateFingerprint": "parent-fixture", "status": "waiting", "mode": "live", "startedAt": now.Format(time.RFC3339Nano), "heartbeatAt": now.Format(time.RFC3339Nano), "waitingOn": map[string]any{"kind": "approval", "subject": approval}})
	write("updateWorkRun", map[string]any{"runId": runID, "rerun": map[string]any{"requestId": rerun, "stepKey": "section", "reason": "rerun"}})
	write("createWorkRun", map[string]any{"runId": child, "parentRunId": runID, "automationName": "child", "templateFingerprint": "child-fixture", "status": "running", "mode": "live", "startedAt": now.Format(time.RFC3339Nano)})
	write("createWorkStep", map[string]any{"stepId": key + "step", "runId": runID, "key": "section", "seq": 0, "stepType": "automation", "status": "running", "attempt": 1, "startedAt": now.Format(time.RFC3339Nano)})
	argsHash, err := permission.Hash("workerHost", "fs_read", map[string]any{"path": "/fixture/file"})
	require.NoError(t, err)
	scope := permission.Scope{Version: 1, Kind: "directory_read", RegistrationID: "fixture-worker", MachineID: "fixture-machine", MachineLabel: "Test machine", Tool: "workerHost", Action: "fs_read", Directory: "/fixture", ArgsHash: argsHash, AllowedModes: []string{"once", "always"}}
	subject := map[string]any{"agentId": "fixture-agent", "computerPermission": scope.Map()}
	write("createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "section", "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": now.Format(time.RFC3339Nano), "expiresAt": now.Add(time.Hour).Format(time.RFC3339Nano)})
	admitted := make(chan work.DispatchRequest, 8)
	var peers []*work.Integration
	var dispatchers []*workRunDispatcher
	var probes []*observedHumanClaim
	for _, eng := range []*memql.MemQLEngine{writer, reader} {
		integration := work.New(eng, eng.Logger, func() *bun.DB { return db })
		integration.SetGateDB(func() *sql.DB { return gates })
		require.NoError(t, eng.RegisterIntegration(integration))
		d := &workRunDispatcher{app: &App{engine: eng, Logger: eng.Logger}}
		probe := &observedHumanClaim{actual: automations.NewClusterExecutionGuard(func() *bun.DB { return db }, eng.Logger).StrictClaimer()}
		integration.SetDispatcher(&humanReadyDispatcher{workRunDispatcher: d, admitted: admitted})
		integration.SetRunClaimer(probe)
		peers, dispatchers, probes = append(peers, integration), append(dispatchers, d), append(probes, probe)
	}
	initial := automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer()
	require.True(t, initial.ClaimWithTTL(ctx, "work.run.dispatch", runID+"#rerun:"+rerun, 0))
	decided := false
	for _, cap := range peers[1].Capabilities() {
		if cap.Name == "decideApproval" {
			_, err := cap.Handler(ctx, map[string]any{"approvalId": approval, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}}, 0)
			require.NoError(t, err)
			decided = true
		}
	}
	require.True(t, decided)
	event := events.Event{Payload: map[string]any{"id": runID, "payload": map[string]any{"status": "running", "automationName": "parent", "goalId": goalID, "ownerUserId": owner, "humanResumeId": approval, "rerun": map[string]any{"requestId": rerun}}}}
	for _, peer := range peers {
		peer.HandleRunEvent(event)
	}
	time.Sleep(2200 * time.Millisecond)
	for _, probe := range probes {
		require.Zero(t, probe.attempts.Load(), "the answer claim must remain unspent before settlement")
	}
	write("updateWorkRun", map[string]any{"runId": child, "status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approval}})
	write("updateWorkStep", map[string]any{"stepId": key + "step", "status": "waiting", "childRunId": child})
	original := work.DispatchRequest{RunId: runID, OwnerUserId: owner, RerunRequestId: rerun}
	staleGeneration := original
	staleGeneration.RerunRequestId = "another-generation"
	for n, d := range dispatchers {
		d.report(ctx, original, nil, &workstate.HumanWait{ApprovalID: "another-answer"})
		d.report(ctx, staleGeneration, nil, &workstate.HumanWait{ApprovalID: approval})
		require.Zero(t, probes[n].attempts.Load(), "stale settlement must not reach a claim")
	}
	// The original execution context may be cancelled while its wait unwinds.
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	for _, d := range dispatchers {
		d.report(stopped, original, nil, &workstate.HumanWait{ApprovalID: approval})
	}
	var resumed work.DispatchRequest
	select {
	case resumed = <-admitted:
	case <-time.After(time.Second):
	}
	// Exercise an actual sweep integration with NO executing dispatcher. In
	// the broken path it abandons this approved run; no manual agent sweep is
	// available to rescue it. A successful resume reports fresh progress.
	var leader *work.Integration
	for _, plugin := range memql.RegisteredPlugins() {
		if plugin.Name == "work" {
			provider, err := plugin.Factory(memql.PluginContext{Engine: reader, Logger: reader.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, AdmitSourceRow: memql.AdmitSourceRow})
			require.NoError(t, err)
			leader = provider.(*work.Integration)
		}
	}
	require.NotNil(t, leader)
	sweepAt := time.Now().Add(2 * time.Minute).UTC()
	leader.SetNow(func() time.Time { return sweepAt })
	if resumed.RunId != "" {
		write("updateWorkRun", map[string]any{"runId": runID, "heartbeatAt": sweepAt.Format(time.RFC3339Nano)})
		write("updateWorkStep", map[string]any{"stepId": key + "step", "status": "running", "attempt": 2})
	}
	_, err = leader.SweepWaiting(auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(context.Background(), "non-executing-sweep-test")), time.Minute)
	require.NoError(t, err)
	j, err := automations.LoadRunJournal(memql.ContextWithFreshRead(ctx), reader, runID)
	require.NoError(t, err)
	require.Equal(t, "running", j.Status, "non-executing sweep abandoned an already-approved, settled invocation")
	require.Equal(t, approval, resumed.HumanResumeId)
	require.Equal(t, rerun, resumed.RerunRequestId)
	require.False(t, resumed.Recovery, "settlement must use the ordinary same-answer claim")
	for n, d := range dispatchers {
		d.report(ctx, original, nil, &workstate.HumanWait{ApprovalID: approval})
		peers[n].HandleRunEvent(event)
	}
	select {
	case duplicate := <-admitted:
		t.Fatalf("settlement/event duplicated the same answer: %+v", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
}
