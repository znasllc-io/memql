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

func TestGuidanceDBRecoveryKeepsReceiptGateThroughCancelledCommit(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	runID := a.openRun(t, nil)
	a.writeVersion(t, runID, runID, "draft", 0, 1, nil, "", map[string]any{"status": "failed"})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"draft"}})
	person := actorCtx(a.owner)
	runContext := common.ContextWithRun(person, common.RunContext{RunId: runID, OwnerUserId: a.owner, StepKey: "draft", Mode: common.RunModeLive})
	id, err := a.i.AskComputerScope(runContext, a.owner, runID, map[string]any{"agentId": "agent-a", "summary": "Inspect a directory", "mode": "observe"})
	require.NoError(t, err)
	answer := map[string]any{"feedback": "Read the repository only."}
	a.write(t, "decideWorkApproval", map[string]any{"approvalId": id, "decision": "answered", "decidedBy": a.owner, "decidedAt": rfc(time.Now()), "answer": answer})
	ctx, cancel := context.WithCancel(person)
	defer cancel()
	wrapper := &permissionCommitEngine{Engine: a.eng, mutation: "updateWorkRun", entered: make(chan struct{}), proceed: make(chan struct{}), cancel: cancel}
	a.i.engine = wrapper
	args := map[string]any{"approvalId": id, "decision": "answered", "answer": answer}
	done := make(chan error, 1)
	go func() { _, err := a.i.handleDecideApproval(ctx, args, 0); done <- err }()
	select {
	case <-wrapper.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not reach run commit")
	}
	other := make(chan error, 1)
	go func() { _, err := b.i.handleDecideApproval(person, args, 0); other <- err }()
	select {
	case err := <-other:
		t.Fatalf("second replica entered before recovered commit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(wrapper.proceed)
	require.NoError(t, <-done)
	require.NoError(t, <-other)
	run, err := b.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
	require.NoError(t, err)
	require.Equal(t, memql.BareShortId(id), rowString(run, "humanResumeId"))
	require.Equal(t, waitKindReplan, rowString(rowMap(run, "waitingOn"), "kind"))
}

// The decision, exact human guidance and replan wait must be visible to a
// replica that did not create the request. Racing the same answer must record
// one decision, while preserving completed output and the original budget.
func TestGuidanceDBPersistsOnceAcrossReplicasWithoutGrantingAccess(t *testing.T) {
	a, b := openActsDB(t), openActsDB(t)
	b.owner = a.owner
	runID := a.openRun(t, nil)
	a.writeVersion(t, runID, runID, "fetch", 0, 1, nil, "saved evidence", nil)
	a.writeVersion(t, runID, runID, "draft", 1, 1, nil, "", map[string]any{"status": "failed"})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "stepOrder": []string{"fetch", "draft"}, "spent": map[string]any{"modelCalls": 4, "retries": 2}})
	runContext := common.ContextWithRun(actorCtx(a.owner), common.RunContext{RunId: runID, OwnerUserId: a.owner, StepKey: "draft", Mode: common.RunModeLive})
	approvalID, err := a.i.AskComputerScope(runContext, a.owner, runID, map[string]any{"agentId": "agent-a", "summary": "Inspect the home directory", "mode": "observe"})
	require.NoError(t, err)
	person := actorCtx(a.owner)
	pending, err := b.i.store().pendingApprovalsForOwner(memql.ContextWithFreshRead(person))
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.True(t, b.i.ApprovalGuidanceAvailable(person, pending[0]))

	guidance := "Inspect /home/alice/projects/memql only, and ask for read access there."
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for n, peer := range []*Integration{a.i, b.i} {
		wg.Add(1)
		go func(n int, peer *Integration) {
			defer wg.Done()
			_, errs[n] = peer.handleDecideApproval(person, map[string]any{"approvalId": approvalID, "decision": "answered", "answer": map[string]any{"feedback": guidance}}, 0)
		}(n, peer)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1]) // An identical retry recovers the recorded answer.
	run, err := b.i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
	require.NoError(t, err)
	require.Equal(t, runStatusWaiting, rowString(run, "status"))
	require.Equal(t, waitKindReplan, rowString(rowMap(run, "waitingOn"), "kind"))
	require.Equal(t, "draft", rowString(rowMap(run, "waitingOn"), "subject"))
	require.Contains(t, rowString(rowMap(run, "waitingOn"), "reason"), guidance)
	require.Equal(t, 4, rowInt(rowMap(run, "spent"), "modelCalls"))
	require.Equal(t, 2, rowInt(rowMap(run, "spent"), "retries"))
	pending, err = a.i.store().pendingApprovalsForOwner(memql.ContextWithFreshRead(person))
	require.NoError(t, err)
	require.Empty(t, pending)
	rows, err := b.i.store().query(memql.ContextWithFreshRead(person), "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "answered", rowString(rows[0], "decision"))
	require.Equal(t, map[string]any{"feedback": guidance}, rowMap(rows[0], "answer"))
	var decisions int
	err = a.db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:work:approval' AND id = ? AND payload->>'decision' = 'answered'`, "v1:work:approval:"+memql.BareShortId(approvalID)).Scan(person, &decisions)
	require.NoError(t, err)
	require.Equal(t, 1, decisions, "an identical retry must not write another answer version")
	require.False(t, b.i.ApprovalGuidanceAvailable(person, rows[0]))
	for _, step := range a.query(t, a.owner, "query "+call("workStepsForOwnerRun", map[string]any{"runId": runID})) {
		if rowString(step, "key") == "fetch" {
			require.Equal(t, "done", rowString(step, "status"))
			require.Equal(t, 1, rowInt(step, "version"))
			require.Equal(t, "saved evidence", rowString(rowMap(step, "result"), "result"))
		}
	}
	_, err = b.i.handleDecideApproval(actorCtx("another-owner"), map[string]any{"approvalId": approvalID, "decision": "approved"}, 0)
	require.Error(t, err)
}
