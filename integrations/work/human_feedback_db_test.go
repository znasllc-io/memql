package work

import (
	"context"
	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"sync"
	"testing"
	"time"
)

func TestHumanFeedbackDBTwoReplicasAskOnceResumeAndRestoreCompletedTools(t *testing.T) {
	db, bff, _, _ := compileDB(t)
	owner := canonicalUser("compile-alice")
	person, cancel := context.WithTimeout(actorCtx(owner), 20*time.Second)
	defer cancel()
	nodes, err := bff.handleCreateGoal(person, map[string]any{"statement": "Use the selected format", "input": map[string]any{}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	receipt := decodeReply(t, nodes)
	runID, goalID := receipt["runId"].(string), receipt["goalId"].(string)
	peers := []*Integration{}
	engines := []*memql.MemQLEngine{}
	for range 2 {
		pool := workHeadsPeer(t, db)
		engine := dispatchDBEngine(t, pool)
		peers = append(peers, New(engine, testLogger(), func() *bun.DB { return pool }))
		engines = append(engines, engine)
	}
	if err = peers[0].store().updateRun(person, runID, map[string]any{"status": "running", "automationName": "test"}); err != nil {
		t.Fatal(err)
	}
	runCtx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: owner, StepKey: "reason", Mode: common.RunModeLive})
	saved := []common.ChatMessage{{Role: "system", Content: "Instructions"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "write", Name: "composeFile"}}}, {Role: "tool", ToolCallId: "write", Content: `{"outputFileId":"already-saved"}`}}
	if err = engines[0].SaveWorkContinuation(runCtx, saved); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for n, peer := range peers {
		wg.Add(1)
		go func(n int, p *Integration) {
			defer wg.Done()
			ids[n], errs[n] = p.AskFeedback(runCtx, owner, runID, "Which format?", "text", nil, time.Time{})
		}(n, peer)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("duplicate/lost question across replicas: %v %v", ids, errs)
	}
	approvals, err := peers[1].store().pendingApprovalsForOwner(person)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("expected one question, got %v %v", approvals, err)
	}
	run, err := peers[0].store().runForOwner(memql.ContextWithFreshRead(person), runID)
	if err != nil || run["status"] != "waiting" {
		t.Fatalf("work did not park: %v %v", run, err)
	}
	if _, err = peers[0].handleDecideApproval(runCtx, map[string]any{"approvalId": ids[0], "decision": "answered", "answer": map[string]any{"text": "PDF"}}, 0); err == nil {
		t.Fatal("agent answered its own question")
	}
	// The initial execution's lease is still held when a person answers.
	initial := automations.NewClusterExecutionGuard(func() *bun.DB { return db }, testLogger()).StrictClaimer()
	if !initial.ClaimWithTTL(person, runClaimName, runID, runClaimTTL) {
		t.Fatal("could not acquire original execution lease")
	}
	nodes, err = peers[1].handleDecideApproval(person, map[string]any{"approvalId": ids[0], "decision": "answered", "answer": map[string]any{"text": "PDF"}}, 0)
	if err != nil || decodeReply(t, nodes)["runResumed"] != true {
		t.Fatalf("answer did not resume: %v %v", nodes, err)
	}
	run, err = peers[0].store().runForOwner(memql.ContextWithFreshRead(person), runID)
	if err != nil || rowString(run, "humanResumeId") != memql.BareShortId(ids[0]) {
		t.Fatalf("answer lost its dispatch identity: %v %v", run, err)
	}
	dispatchers := []*capturingDispatcher{{}, {}}
	for n, peer := range peers {
		peer.SetDispatcher(dispatchers[n])
		peer.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, testLogger()).StrictClaimer())
	}
	req, ok := runEventFields(remedyEvent(merged(run, map[string]any{"id": runID})))
	if !ok || req.HumanResumeId == "" {
		t.Fatal("run event lost answer identity")
	}
	for range 3 {
		for _, peer := range peers {
			wg.Add(1)
			go func(p *Integration) { defer wg.Done(); p.dispatchRun(person, req) }(peer)
		}
		wg.Wait()
	}
	if got := len(dispatchers[0].seen()) + len(dispatchers[1].seen()); got != 1 {
		t.Fatalf("answer dispatched %d times across two replicas with original lease held; want exactly once", got)
	}
	journal, err := automations.LoadRunJournal(person, engines[1], runID)
	if err != nil || journal.HumanResumeId != req.HumanResumeId {
		t.Fatalf("execution replica lost answer identity: %v %v", journal, err)
	}
	id, err := peers[0].AskFeedback(runCtx, owner, runID, "Which format?", "text", nil, time.Time{})
	if err != nil || id != ids[0] {
		t.Fatalf("resume asked again: %s %v", id, err)
	}
	restored, err := engines[1].RestoreWorkContinuation(runCtx, []common.ChatMessage{{Role: "user", Content: "[Recorded answer for this question] PDF"}})
	if err != nil || len(restored) != 4 || restored[2].Content != saved[2].Content || restored[3].Content != "[Recorded answer for this question] PDF" {
		t.Fatalf("completed effect or answer lost crossing replica: %+v %v", restored, err)
	}
}
