package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

func TestReviewGoalBootstrapSurvivesReplicaRetries(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	ctx := actorCtx(first.owner)
	goal := DirectGoal{OwnerUserId: first.owner, Statement: "Revise the selected paragraph", AutomationName: "materializeFile", RequestedVia: "library", TriggeredBy: "document-review", Input: map[string]any{"requestId": "captured-proposal"}, Ceilings: map[string]any{"maxModelCalls": 1}}
	proposal := map[string]any{"artifactId": "owned-document", "revision": "file:1", "change": "Use plain language"}
	open := func(i *Integration, key string) (ReviewGoalReceipt, error) {
		return i.OpenReviewGoal(ctx, key, goal, proposal, "Create a revised draft from this feedback?")
	}
	var receipts [2]ReviewGoalReceipt
	var failures [2]error
	var wg sync.WaitGroup
	for n, i := range []*Integration{first.i, second.i} {
		wg.Add(1)
		go func(n int, i *Integration) { defer wg.Done(); receipts[n], failures[n] = open(i, "one-request") }(n, i)
	}
	wg.Wait()
	for _, err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if receipts[0] != receipts[1] {
		t.Fatalf("replicas opened different requests: %+v", receipts)
	}
	receipt := receipts[0]
	readRun := func(id string) map[string]any {
		t.Helper()
		row, err := second.i.store().runForOwner(memql.ContextWithFreshRead(ctx), id)
		if err != nil || row == nil {
			t.Fatalf("run: %v %v", row, err)
		}
		return row
	}
	run := readRun(receipt.RunID)
	if rowString(run, "status") != runStatusWaiting || rowString(rowMap(run, "waitingOn"), "subject") != receipt.ApprovalID {
		t.Fatalf("run was eligible before review: %v", run)
	}
	if (DispatchRequest{Status: runStatusRunning}).CanDispatchStoredRun(rowString(run, "goalId"), rowString(run, "status"), rowMap(run, "waitingOn"), time.Now()) {
		t.Fatal("a stale running event could dispatch the unapproved request")
	}
	for concept, id := range map[string]string{goalConcept: receipt.GoalID, runConcept: receipt.RunID, approvalConcept: receipt.ApprovalID} {
		count, err := first.db.NewSelect().TableExpr(`"MemoryNodes"`).Where("concept = ?", concept).Where("id IN (?, ?)", id, concept+":"+id).Count(context.Background())
		if err != nil || count != 1 {
			t.Fatalf("%s duplicate versions: %d %v", concept, count, err)
		}
	}
	proposal["change"] = "Different instructions"
	if _, err := open(second.i, "one-request"); err == nil {
		t.Fatal("same key accepted a changed proposal")
	}
	proposal["change"] = "Use plain language"
	if err := first.i.store().decideApprovalRow(ctx, receipt.ApprovalID, "approved", first.owner, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := open(second.i, "one-request"); err != nil {
		t.Fatal(err)
	}
	approval, err := one(second.i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workApprovalForOwner", map[string]any{"approvalId": receipt.ApprovalID})))
	if err != nil || rowString(approval, "decision") != "approved" {
		t.Fatalf("retry reset a decision: %v %v", approval, err)
	}
	other, err := one(second.i.store().query(actorCtx("unrelated-reviewer"), "query "+call("workApprovalForOwner", map[string]any{"approvalId": receipt.ApprovalID})))
	if err == nil && other != nil {
		t.Fatal("another person read the proposal")
	}

	// Each crash boundary is recoverable on another replica without creating
	// another goal/run or releasing an unapproved run.
	for _, mutation := range []string{"createWorkRun", "createWorkApproval"} {
		t.Run(mutation, func(t *testing.T) {
			original := first.i.engine
			first.i.engine = reviewGoalWriteFailure{original, mutation}
			broken, err := open(first.i, "failure-"+mutation)
			first.i.engine = original
			if err == nil {
				t.Fatal("injected write failure was ignored")
			}
			recovered, err := open(second.i, "failure-"+mutation)
			if err != nil || broken != recovered {
				t.Fatalf("partial bootstrap: %+v %+v %v", broken, recovered, err)
			}
			if rowString(readRun(recovered.RunID), "status") != runStatusWaiting {
				t.Fatal("recovery released unapproved work")
			}
		})
	}
	// A retry after completion serves the receipt instead of resetting the run.
	if err := first.i.store().updateRun(ctx, receipt.RunID, map[string]any{"status": "succeeded", "finishedAt": rfc(time.Now())}); err != nil {
		t.Fatal(err)
	}
	if _, err := open(second.i, "one-request"); err != nil {
		t.Fatal(err)
	}
	if rowString(readRun(receipt.RunID), "status") != "succeeded" {
		t.Fatal("completed run restarted")
	}
}

type reviewGoalWriteFailure struct {
	Engine
	mutation string
}

func (e reviewGoalWriteFailure) Execute(ctx context.Context, statement string) (*memql.ExecuteResult, error) {
	if strings.Contains(statement, e.mutation+"(") {
		return nil, fmt.Errorf("%s: %w", e.mutation, errors.New("injected failure"))
	}
	return e.Engine.Execute(ctx, statement)
}

func TestReviewApprovalDecisionsSerializeAcrossReplicas(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	ctx := actorCtx(first.owner)
	receipt, err := first.i.OpenReviewGoal(ctx, "decision-race", DirectGoal{OwnerUserId: first.owner, Statement: "Revise a document", AutomationName: "materializeFile", RequestedVia: "library"}, map[string]any{"revision": "file:1"}, "Approve this exact revision?")
	if err != nil {
		t.Fatal(err)
	}
	// Both decision clocks precede the proposal's actual saved revision.
	first.i.SetNow(func() time.Time { return time.Now().Add(-time.Hour) })
	second.i.SetNow(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for n, i := range []*Integration{first.i, second.i} {
		wg.Add(1)
		go func(n int, i *Integration) {
			defer wg.Done()
			_, err := i.handleDecideApproval(ctx, map[string]any{"approvalId": receipt.ApprovalID, "decision": []string{"approved", "rejected"}[n]}, 0)
			outcomes <- err
		}(n, i)
	}
	wg.Wait()
	close(outcomes)
	succeeded := 0
	for err := range outcomes {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d competing decisions succeeded; wanted one", succeeded)
	}
	current, err := one(second.i.store().query(memql.ContextWithFreshRead(ctx), "query "+call("workApprovalForOwner", map[string]any{"approvalId": receipt.ApprovalID})))
	if err != nil || rowString(current, "decision") == "" {
		t.Fatalf("decision was older than the pending proposal: %v %v", current, err)
	}
	run, err := second.i.store().runForOwner(memql.ContextWithFreshRead(ctx), receipt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	want := runStatusRunning
	if rowString(current, "decision") == "rejected" {
		want = runStatusFailed
	}
	if rowString(run, "status") != want {
		t.Fatalf("decision did not control the same run: %v", run)
	}
}
