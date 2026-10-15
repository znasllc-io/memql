//go:build agent

package app

import (
	"context"
	"database/sql"
	"fmt"
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

// A real earlier permission decision must not turn a later public rerun into
// another dispatch of that consumed answer. Replica B takes the user's act;
// either fresh peer can claim its new request without browser-local state.
func TestWorkDBPublicRerunAfterConsumedPermissionDispatchesOnPeer(t *testing.T) {
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	key := id.NewShortId()
	owner, runID, goalID, approval := key+"owner", key+"run", key+"goal", key+"permission"
	person := auth.ContextWithAccess(auth.ContextWithUserActor(context.Background(), owner), &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	write := func(name string, args map[string]any) {
		templateMutation(t, writer, auth.ContextWithInternalOrigin(person), name, args)
	}
	load := func(engine *memql.MemQLEngine) *automations.RunJournal {
		journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), engine, runID)
		require.NoError(t, err)
		return journal
	}
	peers := []*work.Integration{}
	admitted := make(chan work.DispatchRequest, 4)
	for _, engine := range []*memql.MemQLEngine{writer, reader} {
		var peer *work.Integration
		for _, plugin := range memql.RegisteredPlugins() {
			if plugin.Name == "work" {
				provider, err := plugin.Factory(memql.PluginContext{Engine: engine, Logger: engine.Logger, BunDB: func() *bun.DB { return db }, GateDB: func() *sql.DB { return gates }, AdmitSourceRow: memql.AdmitSourceRow})
				require.NoError(t, err)
				peer = provider.(*work.Integration)
			}
		}
		require.NotNil(t, peer)
		peer.SetDispatcher(&humanReadyDispatcher{workRunDispatcher: &workRunDispatcher{app: &App{engine: engine, Logger: engine.Logger}}, admitted: admitted})
		peer.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, engine.Logger).StrictClaimer())
		peers = append(peers, peer)
	}
	act := func(name string, args map[string]any) {
		for _, capability := range peers[1].Capabilities() {
			if capability.Name == name {
				_, err := capability.Handler(person, args, 0)
				require.NoError(t, err)
				return
			}
		}
		t.Fatalf("missing public capability %s", name)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic rerun after a completed permission", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "automationName": "fixture", "templateFingerprint": "fixture", "status": "waiting", "mode": "live", "startedAt": now, "waitingOn": map[string]any{"kind": "approval", "subject": approval}})
	write("createWorkStep", map[string]any{"stepId": key + "prefix", "runId": runID, "key": "prefix", "seq": 0, "stepType": "function", "status": "waiting", "attempt": 1, "version": 1, "startedAt": now})
	argsHash, err := permission.Hash("workerHost", "fs_read", map[string]any{"path": "/fixture/file"})
	require.NoError(t, err)
	scope := permission.Scope{Version: 1, Kind: "directory_read", RegistrationID: "fixture-worker", MachineID: "fixture-machine", MachineLabel: "Test machine", Tool: "workerHost", Action: "fs_read", Directory: "/fixture", ArgsHash: argsHash, AllowedModes: []string{"once", "always"}}
	subject := map[string]any{"agentId": "fixture-agent", "computerPermission": scope.Map()}
	write("createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "prefix", "kind": "scopeElevation", "subject": subject, "artifactHash": workstate.ArtifactHash(subject), "requestedAt": now, "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	act("decideApproval", map[string]any{"approvalId": approval, "decision": "approved", "answer": map[string]any{"permissionMode": "once"}})
	require.Equal(t, approval, load(writer).HumanResumeId)
	write("updateWorkStep", map[string]any{"stepId": key + "prefix", "status": "done", "result": map[string]any{"stepId": "prefix", "status": "completed", "value": "retained result"}})
	write("createWorkStep", map[string]any{"stepId": key + "tests", "runId": runID, "key": "tests", "seq": 1, "stepType": "automation", "status": "failed", "attempt": 1, "version": 1, "startedAt": now})
	head := workstate.Head{"prefix": {Version: 1}, "tests": {Version: 1}}
	write("updateWorkRun", map[string]any{"runId": runID, "status": "abandoned", "stepOrder": []string{"prefix", "tests"}, "head": head.Object(), "spent": map[string]any{"modelCalls": 48, "retries": 2}, "finishedAt": now})
	prior := load(reader) // Warm the peer with the old answered marker.
	require.Equal(t, approval, prior.HumanResumeId)
	act("rerunStep", map[string]any{"runId": runID, "stepKey": "tests"})
	current := load(writer)
	require.NotNil(t, current.Rerun)
	require.Equal(t, map[string]int{"tests": 2}, current.Rerun.Versions)
	require.Equal(t, head, current.Head)
	require.Equal(t, "retained result", current.Steps["prefix"].Value)
	var calls, retries int
	require.NoError(t, db.NewRaw(`SELECT (payload->'spent'->>'modelCalls')::int, (payload->'spent'->>'retries')::int FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(person, &calls, &retries))
	require.Equal(t, 48, calls)
	require.Equal(t, 2, retries)
	var decision, mode string
	require.NoError(t, db.NewRaw(`SELECT payload->>'decision', payload->'answer'->>'permissionMode' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:approval:"+approval).Scan(person, &decision, &mode))
	require.Equal(t, "approved", decision)
	require.Equal(t, "once", mode, "a new execution must not widen or erase the original permission")
	claimer := automations.NewClusterExecutionGuard(func() *bun.DB { return db }, writer.Logger).StrictClaimer()
	require.True(t, claimer.ClaimWithTTL(person, "work.run.dispatch", runID, 4*time.Minute))
	require.True(t, claimer.ClaimWithTTL(person, "work.run.dispatch", runID+"#answer:"+approval+"#rerun:", 0))
	event := events.Event{Payload: map[string]any{"id": runID, "payload": map[string]any{"status": "running", "automationName": current.AutomationName, "goalId": current.GoalId, "ownerUserId": current.OwnerUserId, "humanResumeId": current.HumanResumeId, "rerun": map[string]any{"requestId": current.Rerun.RequestId}}}}
	for range 2 {
		for _, peer := range peers {
			peer.HandleRunEvent(event)
		}
	}
	select {
	case req := <-admitted:
		require.Empty(t, current.HumanResumeId, "a fresh request must not wait for the consumed permission")
		require.Empty(t, req.HumanResumeId)
		require.Equal(t, current.Rerun.RequestId, req.RerunRequestId)
		require.True(t, workRerunServable(req, load(reader)))
		// A late answer event and an older explicit rerun cannot borrow this claim.
		stale := req
		stale.HumanResumeId = approval
		require.False(t, workRerunServable(stale, current))
		_, ready := (&workRunDispatcher{app: &App{engine: reader, Logger: reader.Logger}}).PrepareDispatch(person, stale)
		require.False(t, ready)
		stale = req
		stale.RerunRequestId = "older-rerun"
		require.False(t, workRerunServable(stale, current))
	case <-time.After(5 * time.Second):
		t.Fatal("new rerun was stranded behind the consumed permission answer")
	}
	require.False(t, claimer.ClaimWithTTL(person, "work.run.dispatch", fmt.Sprintf("%s#rerun:%s", runID, current.Rerun.RequestId), 0), "the new request, not the prior answer, owns the claim")
	select {
	case req := <-admitted:
		t.Fatalf("peer replayed the same request: %+v", req)
	case <-time.After(100 * time.Millisecond):
	}
}
