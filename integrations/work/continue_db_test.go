package work

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

func TestContinueFinishedResultDBAcrossReplicas(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	b.owner = a.owner
	template := installedReplanRun(t)
	runID := a.openRun(t, map[string]any{"automationName": template["automationName"], "templateFingerprint": template["templateFingerprint"]})
	a.writeVersion(t, runID, runID, "goalInput", 0, 1, nil, "saved input", nil)
	a.writeVersion(t, runID, runID, "reason", 1, 1, nil, "saved result; requested effect still missing", nil)
	a.finishRun(t, runID, []string{"goalInput", "reason"}, workstate.Head{"goalInput": {Version: 1}, "reason": {Version: 1}})
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "spent": map[string]any{"modelCalls": 300, "tokens": 4567, "wallClockMs": 30000000, "retries": 2}})
	before := a.runRow(t, runID)
	goalID := rowString(before, "goalId")
	person := actorCtx(a.owner)
	args := map[string]any{"runId": runID, "runVersion": testRunVersion(t, before), "requestId": "same-user-feedback", "feedback": "Deliver the missing output from the saved work."}
	// Both replicas read the same finished state; only one lifecycle-gated
	// write may request continuation and renew the execution window.
	_, err := b.i.readActRun(person, runID)
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for n, peer := range []*Integration{a.i, b.i} {
		wg.Add(1)
		go func(n int, peer *Integration) { defer wg.Done(); _, errs[n] = peer.handleContinueRun(person, args, 0) }(n, peer)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	after := b.runRow(t, runID)
	require.Equal(t, runStatusWaiting, rowString(after, "status"))
	for _, field := range []string{"spent", "head", "stepOrder", "goalId", "templateFingerprint"} {
		require.Equal(t, before[field], after[field], field)
	}
	state := rowMap(rowMap(after, "classification"), "progressContinuation")
	require.Equal(t, "result:same-user-feedback", rowString(state, "feedbackId"))
	goal := a.query(t, a.owner, "query "+call("workGoalForOwner", map[string]any{"goalId": goalID}))[0]
	limits, err := progressCeilings(goal, after, workstate.Ceilings{})
	require.NoError(t, err)
	require.Nil(t, workstate.CheckCeilings(limits, workstate.Spent{ModelCalls: 301, Tokens: 4567, WallClockMs: 30000001}, 0))
	ctx, err := b.i.LoadReplanContext(person, b.owner, runID, "reason")
	require.NoError(t, err)
	require.Len(t, ctx.CompletedSteps, 2)
	require.Equal(t, "done", ctx.Recorded["reason"])
	require.Equal(t, true, ctx.FailedStep["finishedResultFeedback"])
	require.Contains(t, ctx.FailedStep["errorMessage"], args["feedback"])
	_, err = a.i.handleContinueRun(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, after, b.runRow(t, runID), "same-request retry must not renew or dispatch twice")
	_, err = b.i.handleAcceptGoalResults(person, map[string]any{"goalId": goalID, "runId": runID, "runVersion": args["runVersion"]}, 0)
	require.ErrorContains(t, err, "still active")
	// The inverse race: owner acceptance is final for automatic continuation.
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusSucceeded})
	latest := a.runRow(t, runID)
	_, err = a.i.handleAcceptGoalResults(person, map[string]any{"goalId": goalID, "runId": runID, "runVersion": testRunVersion(t, latest)}, 0)
	require.NoError(t, err)
	_, err = b.i.handleContinueRun(person, args, 0)
	require.ErrorContains(t, err, "open, unaccepted")
	closed, err := b.i.store().goalForOwner(memql.ContextWithFreshRead(person), goalID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed["status"])
}

func TestContinueConflictDBFindsOlderActiveRunBeyondHistoryPage(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	b.owner = a.owner
	runID := a.openRun(t, nil)
	goalID := rowString(a.runRow(t, runID), "goalId")
	// More terminal rows than a default history page must not hide an older
	// active run. Existence is filtered before pagination and returns one row.
	for n := 0; n < 105; n++ {
		a.write(t, "createWorkRun", map[string]any{"runId": newRowId(runConcept), "goalId": goalID, "automationName": "work", "templateFingerprint": "synthetic-history", "status": runStatusAbandoned, "mode": modeLive, "startedAt": rfc(time.Now())})
	}
	latestID := a.openRun(t, map[string]any{"goalId": goalID, "status": runStatusSucceeded})
	latest := a.runRow(t, latestID)
	args := map[string]any{"goalId": goalID, "runVersion": testRunVersion(t, latest)}
	rows := b.query(t, a.owner, "query "+call("workContinuationConflictsForOwnerGoal", args))
	require.Len(t, rows, 1)
	require.Equal(t, memql.BareShortId(runID), memql.BareShortId(rowString(rows[0], "id")))
	a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": runStatusAbandoned})
	// Refresh the latest result: an abandoned older execution is terminal.
	a.write(t, "updateWorkRun", map[string]any{"runId": latestID, "status": runStatusSucceeded})
	args["runVersion"] = testRunVersion(t, a.runRow(t, latestID))
	rows = b.query(t, a.owner, "query "+call("workContinuationConflictsForOwnerGoal", args))
	require.Empty(t, rows)
}

// Intercept the real engine write, after the run has committed. This models
// lost receipt storage without replacing database semantics or the replica gate.
type resultFeedbackFaultEngine struct {
	Engine
	t         *testing.T
	failSeal  bool
	runWrites int
}

func (e *resultFeedbackFaultEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	c := recordedCall{Query: q}
	if c.Name() == "updateWorkRun" {
		e.runWrites++
	}
	if c.Name() == "createWorkObservation" && e.failSeal && rowMap(rowMap(c.Args(e.t), "data"), "resultFeedback")["accepted"] == true {
		return nil, fmt.Errorf("injected acceptance seal outage")
	}
	return e.Engine.Execute(ctx, q)
}

func TestContinueReceiptDBRecoversPostCommitFailureAcrossSkewedReplicas(t *testing.T) {
	a, b := freshActsDBPair(t, openActsDB(t), openActsDB(t))
	template := installedReplanRun(t)
	run := a.openRun(t, map[string]any{"automationName": template["automationName"], "templateFingerprint": template["templateFingerprint"]})
	a.writeVersion(t, run, run, "goalInput", 0, 1, nil, "input", nil)
	a.writeVersion(t, run, run, "reason", 1, 1, nil, "result", nil)
	a.finishRun(t, run, []string{"goalInput", "reason"}, workstate.Head{"goalInput": {Version: 1}, "reason": {Version: 1}})
	person := actorCtx(a.owner)
	fault := &resultFeedbackFaultEngine{Engine: a.i.engine, t: t, failSeal: true}
	a.i.engine = fault
	args := map[string]any{"runId": run, "runVersion": testRunVersion(t, a.runRow(t, run)), "requestId": "api-first", "feedback": "Deliver the saved output."}
	nodes, err := a.i.handleContinueRun(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, true, decodeReply(t, nodes)["continuationRequested"])
	require.Equal(t, "pending", decodeReply(t, nodes)["feedbackReceiptState"])
	committed := a.runRow(t, run)
	require.Equal(t, runStatusWaiting, committed["status"])
	require.Equal(t, 1, fault.runWrites)
	receipt, accepted, intentAt, err := b.i.readResultFeedback(memql.ContextWithFreshRead(person), run, "api-first")
	require.NoError(t, err)
	require.False(t, accepted)
	require.NotNil(t, receipt)
	humanAt := receipt["requestedAt"]
	// The repair replica is behind the intent's timestamp. Its accepted row must
	// still sort later, preserving both human time and the original dispatch.
	b.i.now = func() time.Time { return intentAt.Add(-time.Hour) }
	nodes, err = b.i.handleContinueRun(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, "sealed", decodeReply(t, nodes)["feedbackReceiptState"])
	require.Equal(t, committed, b.runRow(t, run), "sealing must not renew or dispatch work")
	receipt, accepted, sealedAt, err := a.i.readResultFeedback(memql.ContextWithFreshRead(person), run, "api-first")
	require.NoError(t, err)
	require.True(t, accepted)
	require.True(t, sealedAt.After(intentAt))
	require.Equal(t, humanAt, receipt["requestedAt"])
	// An intent retry must never regress an already accepted row.
	require.NoError(t, b.i.writeResultFeedback(memql.ContextWithFreshRead(person), run, receipt, false))
	_, accepted, sameAt, err := a.i.readResultFeedback(memql.ContextWithFreshRead(person), run, "api-first")
	require.NoError(t, err)
	require.True(t, accepted)
	require.Equal(t, sealedAt, sameAt)
	// A second direct API continuation keeps the first accepted direction and
	// makes a retry of either request harmless even after the latest field moves.
	fault.failSeal = false
	b.i.now = time.Now
	a.write(t, "updateWorkRun", map[string]any{"runId": run, "status": runStatusSucceeded})
	second := maps.Clone(args)
	second["requestId"] = "api-second"
	second["feedback"] = "Retain its final proof."
	second["runVersion"] = testRunVersion(t, a.runRow(t, run))
	_, err = b.i.handleContinueRun(person, second, 0)
	require.NoError(t, err)
	latest := a.runRow(t, run)
	_, err = a.i.handleContinueRun(person, args, 0)
	require.NoError(t, err)
	require.Equal(t, latest, b.runRow(t, run))
	rows := a.query(t, a.owner, "query "+call("workResultFeedbackForOwnerRun", map[string]any{"runId": run}))
	require.Len(t, rows, 2)
	require.Equal(t, []string{"api-first", "api-second"}, []string{rowString(rowMap(rowMap(rows[0], "data"), "resultFeedback"), "requestId"), rowString(rowMap(rowMap(rows[1], "data"), "resultFeedback"), "requestId")})
}
