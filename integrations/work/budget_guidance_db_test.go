package work

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// Boot the installed DSL on two independent replicas once. Each scenario
// retains separate integration state, write-gate pools and a unique owner;
// only the immutable DSL and the replica-local query caches are reused.
// This follows TestWorkVersionActs without turning the cross-replica seam
// into two handles for a single engine.
func TestBudgetGuidanceDBWorkflowAcrossReplicas(t *testing.T) {
	replicas := [2]actsDB{openActsDB(t), openActsDB(t)}
	cases := []struct {
		name string
		run  func(*testing.T, actsDB, actsDB)
	}{
		{"PersistsOnceAndOnlyExplicitApprovalResumes", testBudgetGuidanceDBPersistsOnceAndOnlyExplicitApprovalResumes},
		{"RecoversApprovedCheckpointAfterRunReleaseFailure", testBudgetGuidanceDBRecoversApprovedCheckpointAfterRunReleaseFailure},
		{"ModelCallBudgetSeesPeerRaisesAndReceiptsBeforeHardExhaustion", testModelCallBudgetDBSeesPeerRaisesAndReceiptsBeforeHardExhaustion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := fmt.Sprintf("dbtest-budget-guidance-%d", time.Now().UnixNano())
			peers := replicas
			for n := range peers {
				peer := &peers[n]
				peer.owner = owner
				peer.i = New(peer.eng, testLogger())
				peer.i.bunDB = func() *bun.DB { return peer.db }
				peer.i.SetGateDB(testGates(t))
				peer.i.admitRow = memql.AdmitSourceRow
			}
			tc.run(t, peers[0], peers[1])
		})
	}
}

// The owner gate, DSL mutation, collapsed reads and replica coordination are
// real. Saving guidance must neither resolve the request nor release its run.
func testBudgetGuidanceDBPersistsOnceAndOnlyExplicitApprovalResumes(t *testing.T, a, b actsDB) {
	previous := auth.InstalledCapabilityCatalog()
	auth.SetCapabilityCatalog(nil)
	t.Cleanup(func() { auth.SetCapabilityCatalog(previous) })
	for _, peer := range []actsDB{a, b} {
		require.NoError(t, peer.eng.RegisterIntegration(peer.i))
	}
	person := actorCtx(a.owner)
	freshRun := func(runID string) map[string]any {
		t.Helper()
		run, err := a.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
		require.NoError(t, err)
		require.NotNil(t, run)
		return run
	}
	approvalID := newRowId(approvalConcept)
	runID := a.openRun(t, map[string]any{"status": "waiting", "waitingOn": map[string]any{"kind": "approval", "subject": approvalID}})
	run := a.runRow(t, runID)
	goalID := rowString(run, "goalId")
	a.write(t, "updateWorkGoal", map[string]any{"goalId": goalID, "ceilings": map[string]any{"maxModelCalls": 12, "maxRetries": 2}})
	a.writeVersion(t, runID, runID, "fetch", 0, 1, nil, "saved evidence", nil)
	a.writeVersion(t, runID, runID, "draft", 1, 1, nil, "", map[string]any{"status": "waiting"})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"fetch", "draft"}, "spent": map[string]any{"modelCalls": 12, "retries": 1}, "rerun": map[string]any{"requestId": "existing-generation"}})
	subject := map[string]any{"ceiling": "modelCalls", "limit": "12", "actual": "12", "resumeKind": "checkpoint", "continuationId": "exact-test-checkpoint"}
	a.write(t, "createWorkApproval", map[string]any{"approvalId": approvalID, "runId": runID, "kind": "budget", "stepKey": "draft", "subject": subject, "artifactHash": artifactHashOf(subject), "requestedAt": rfc(time.Now())})
	before := a.runRow(t, runID)
	// Warm a stale read on the other replica before the saved feedback exists.
	_, err := b.i.store().query(person, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	require.NoError(t, err)
	args := map[string]any{"approvalId": approvalID, "decision": "answered", "answer": map[string]any{"feedback": "Keep the completed read and use its result."}}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for n, peer := range []*Integration{a.i, b.i} {
		wg.Add(1)
		go func(n int, peer *Integration) {
			defer wg.Done()
			_, errs[n] = peer.handleDecideApproval(person, args, 0)
		}(n, peer)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	after := freshRun(runID)
	for _, field := range []string{"status", "waitingOn", "spent", "rerun", "head", "stepOrder", "humanResumeId"} {
		require.Equal(t, before[field], after[field], field)
	}
	state := discussionExecute(t, b.eng, person, "askApprovalDiscussion", map[string]any{"approvalId": approvalID})
	require.Equal(t, "deferred", state["guidanceMode"])
	require.Equal(t, "Keep the completed read and use its result.", state["savedGuidance"])
	require.Equal(t, true, state["canSendGuidance"])
	require.Equal(t, "pending", rowString(rowMap(state, "approval"), "status"))
	var saves int
	require.NoError(t, a.db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE id = ? AND payload->'answer'->>'feedback' IS NOT NULL`, approvalID).Scan(person, &saves))
	require.Equal(t, 1, saves, "identical concurrent saves append one durable version")
	goal, err := b.i.store().goalForOwner(memql.ContextWithFreshRead(person), goalID)
	require.NoError(t, err)
	require.Equal(t, 12, rowInt(rowMap(goal, "ceilings"), "maxModelCalls"))
	_, err = b.i.handleDecideApproval(actorCtx("another-owner"), args, 0)
	require.Error(t, err)

	// A replacement and explicit approval race under the same receipt gate.
	// A successful replacement must be the text the approving replica copies;
	// a late replacement must fail instead of rewriting an accepted decision.
	newText := "Use the completed read, then finish the response."
	var saveErr, approveErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, saveErr = a.i.handleDecideApproval(person, map[string]any{"approvalId": approvalID, "decision": "answered", "answer": map[string]any{"feedback": newText}}, 0)
	}()
	go func() {
		defer wg.Done()
		_, approveErr = b.i.handleDecideApproval(person, map[string]any{"approvalId": approvalID, "decision": "approved", "answer": map[string]any{"newLimit": 15}}, 0)
	}()
	wg.Wait()
	require.NoError(t, approveErr)
	rows, err := a.i.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	decision := rows[0]
	require.Equal(t, "approved", rowString(decision, "decision"))
	if saveErr == nil {
		require.Equal(t, newText, rowString(rowMap(decision, "answer"), "feedback"))
	} else {
		require.Equal(t, "Keep the completed read and use its result.", rowString(rowMap(decision, "answer"), "feedback"))
	}
	require.Equal(t, 15, rowInt(rowMap(decision, "answer"), "newLimit"))
	after = freshRun(runID)
	require.Equal(t, "running", after["status"])
	require.Equal(t, before["spent"], after["spent"])
	require.Equal(t, before["rerun"], after["rerun"])
	require.Equal(t, before["head"], after["head"])
	require.Equal(t, before["stepOrder"], after["stepOrder"])
	_, err = a.i.handleDecideApproval(person, args, 0)
	require.Error(t, err, "saved guidance cannot overwrite a decided approval")
}
