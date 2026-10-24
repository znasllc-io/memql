package work

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

func progressFixture(t *testing.T) (*Integration, *recordingEngine, map[string]any, common.RunContext) {
	t.Helper()
	i, e := newTestIntegration(t)
	answerRows(e, map[string]any{"maxModelCalls": 96, "wallClockMs": 7200000, "costCeiling": 0.5, "tokenBudget": 1000})
	run := map[string]any{"id": ceilingRunId, "ownerUserId": ceilingOwner, "goalId": ceilingGoalId, "status": runStatusRunning, "startedAt": rfc(testNow), "classification": map[string]any{"workload": "lookup"}}
	e.reply("workRunForOwner", run)
	e.reply("workProgressReceiptsForOwnerRun", progressReceipt("first"))
	return i, e, run, aGoalBackedRun()
}
func progressReceipt(value string) map[string]any {
	return map[string]any{"createdAt": rfc(testNow.Add(time.Second)), "runId": ceilingRunId, "ownerUserId": ceilingOwner, "kind": "tool_result", "content": "verified result", "data": map[string]any{"tool": "readSource", "args": `{"id":"` + value + `"}`, "nativeResultEvidence": workstate.ArtifactHash(map[string]any{"content": value}), "isError": false}}
}
func TestProgressContinuationsRequireNewWorkBeyondLifetimeCaps(t *testing.T) {
	i, e, run, rc := progressFixture(t)
	ctx := context.Background()
	for n := 0; n < 20; n++ {
		e.reply("workProgressReceiptsForOwnerRun", progressReceipt(string(rune('a'+n))))
		continued, err := i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12 * (n + 1)}, 0)
		require.NoError(t, err)
		require.True(t, continued)
		updates := e.callsTo("updateWorkRun")
		run["classification"] = updates[len(updates)-1].Args(t)["classification"]
		// The same spent value on another peer cannot consume another window.
		peer := New(e, testLogger())
		peer.decisionGate = i.decisionGate
		continued, err = peer.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12 * (n + 1)}, 0)
		require.NoError(t, err)
		require.True(t, continued)
		require.Len(t, e.callsTo("updateWorkRun"), n+1)
		// Repeating the same completed call is not new progress.
		continued, err = i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 12 * (n + 2)}, 0)
		require.NoError(t, err)
		require.False(t, continued)
	}
	e.reply("workProgressReceiptsForOwnerRun", progressReceipt("twenty-first"))
	continued, err := i.ContinueWorkProgress(ctx, rc, workstate.Spent{ModelCalls: 252}, 0)
	require.NoError(t, err)
	require.True(t, continued)
	require.Empty(t, e.callsTo("createWorkApproval"))
	require.Empty(t, e.callsTo("updateWorkGoal"))
}
func TestProgressNeverWeakensHardLimitsOrFailedReads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spent   workstate.Spent
		tokens  int
		mutate  func(*recordingEngine, map[string]any, *common.RunContext)
		wantErr bool
	}{
		{name: "money", spent: workstate.Spent{ModelCalls: 12, Cost: 0.5}},
		{name: "tokens", spent: workstate.Spent{ModelCalls: 12, Tokens: 990}, tokens: 20},
		{name: "cancelled", spent: workstate.Spent{ModelCalls: 12}, mutate: func(_ *recordingEngine, r map[string]any, _ *common.RunContext) { r["cancelRequested"] = true }},
		{name: "replay", spent: workstate.Spent{ModelCalls: 12}, mutate: func(_ *recordingEngine, _ map[string]any, rc *common.RunContext) { rc.Mode = common.RunModeReplay }},
		{name: "wrong goal", spent: workstate.Spent{ModelCalls: 12}, mutate: func(_ *recordingEngine, r map[string]any, _ *common.RunContext) { r["goalId"] = "other" }},
		{name: "receipts unavailable", spent: workstate.Spent{ModelCalls: 12}, mutate: func(e *recordingEngine, _ map[string]any, _ *common.RunContext) {
			e.refuse("workProgressReceiptsForOwnerRun", errors.New("offline"))
		}, wantErr: true},
		{name: "wrong owner receipt", spent: workstate.Spent{ModelCalls: 12}, mutate: func(e *recordingEngine, _ map[string]any, _ *common.RunContext) {
			v := progressReceipt("other")
			v["ownerUserId"] = "different"
			e.reply("workProgressReceiptsForOwnerRun", v)
		}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, e, r, rc := progressFixture(t)
			if tc.mutate != nil {
				tc.mutate(e, r, &rc)
			}
			ok, err := i.ContinueWorkProgress(context.Background(), rc, tc.spent, tc.tokens)
			require.Equal(t, tc.wantErr, err != nil)
			require.False(t, ok)
			require.Empty(t, e.callsTo("updateWorkRun"))
			require.Empty(t, e.callsTo("updateWorkGoal"))
		})
	}
}
func TestProgressReceiptRejectsFailuresDiscoveryAndEquivalentRepeats(t *testing.T) {
	a := progressReceipt("one")
	b := progressReceipt("one")
	rowMap(a, "data")["args"] = `{"a":1,"b":2}`
	rowMap(b, "data")["args"] = `{ "b": 2, "a": 1 }`
	require.Equal(t, progressReceiptKey(a), progressReceiptKey(b))
	for _, key := range []string{"isError", "argsTruncated"} {
		v := progressReceipt("one")
		rowMap(v, "data")[key] = true
		require.Empty(t, progressReceiptKey(v))
	}
	for _, name := range []string{"", "discoverCapabilities", "workCapabilities", "recallWorkHistory", "requestUserFeedback", "respondToUser"} {
		v := progressReceipt("one")
		rowMap(v, "data")["tool"] = "scope." + name
		require.Empty(t, progressReceiptKey(v))
	}
	v := progressReceipt("one")
	rowMap(v, "data")["args"] = "broken JSON"
	require.Empty(t, progressReceiptKey(v))
	delete(rowMap(v, "data"), "args")
	require.NotEmpty(t, progressReceiptKey(v), "a successful source query can take no arguments")
}
func TestProgressLimitsIntersectAndDoNotOverflow(t *testing.T) {
	_, overflow := progressCeilings(map[string]any{}, map[string]any{"classification": map[string]any{"progressContinuation": map[string]any{"extensions": math.MaxInt}}}, workstate.Ceilings{})
	require.Error(t, overflow)
	for count := 0; count <= 16; count++ {
		goal := map[string]any{"ceilings": map[string]any{"maxModelCalls": 50, "wallClockMs": 1500000}}
		run := map[string]any{"classification": map[string]any{"workload": "lookup", "progressContinuation": map[string]any{"extensions": count}}}
		limits, err := progressCeilings(goal, run, workstate.Ceilings{MaxModelCalls: 50, WallClockMs: 1500000})
		require.NoError(t, err)
		require.Equal(t, 12*(count+1), limits.MaxModelCalls)
		require.Equal(t, int64(600000*(count+1)), limits.WallClockMs)
	}
}
func TestProgressInFlightHasNoGoalLifetimeDeadline(t *testing.T) {
	_, e, r, rc := progressFixture(t)
	first, peer := NewRunCeilings(e, testLogger()), NewRunCeilings(e, testLogger())
	for _, guard := range []*RunCeilings{first, peer} {
		guard.now = func() time.Time { return testNow }
		deadline, err := guard.Deadline(context.Background(), rc)
		require.NoError(t, err)
		require.True(t, deadline.IsZero())
	}
	r["humanWaitMs"] = 60000
	deadline, err := peer.Deadline(context.Background(), rc)
	require.NoError(t, err)
	require.True(t, deadline.IsZero(), "human waits cannot create a lifetime deadline")
	first.Charge(context.Background(), rc, memql.ModelSpend{Served: "local"})
	require.Equal(t, 1, first.Spent(context.Background(), rc).ModelCalls)
}

func TestProgressLongOperationConsumesBoundedWindows(t *testing.T) {
	i, e, _, rc := progressFixture(t)
	ok, err := i.ContinueWorkProgress(context.Background(), rc, workstate.Spent{WallClockMs: 1500000}, 0)
	require.NoError(t, err)
	require.True(t, ok)
	state := rowMap(rowMap(e.callsTo("updateWorkRun")[0].Args(t), "classification"), "progressContinuation")
	require.Equal(t, 2, rowInt(state, "extensions"))
}
func TestProgressCanRenewAfterDaysWithoutALifetimeLimit(t *testing.T) {
	for _, hard := range []int64{0, 7200000} {
		i, e, _, rc := progressFixture(t)
		e.reply("workGoalForOwner", map[string]any{"id": ceilingGoalId, "ceilings": map[string]any{"wallClockMs": hard}})
		ok, err := i.ContinueWorkProgress(context.Background(), rc, workstate.Spent{WallClockMs: int64((72 * time.Hour) / time.Millisecond)}, 0)
		require.NoError(t, err)
		require.True(t, ok)
		require.Len(t, e.callsTo("updateWorkRun"), 1)
	}
}

func TestProgressFeedbackEpochDoesNotRecycleOldEvidence(t *testing.T) {
	i, e, run, rc := progressFixture(t)
	run["classification"].(map[string]any)["progressContinuation"] = map[string]any{"receiptAfter": rfc(testNow), "extensions": 1}
	old := progressReceipt("before-answer")
	old["createdAt"] = rfc(testNow.Add(-time.Minute))
	e.reply("workProgressReceiptsForOwnerRun", old)
	ok, err := i.ContinueWorkProgress(context.Background(), rc, workstate.Spent{ModelCalls: 24}, 0)
	require.NoError(t, err)
	require.False(t, ok)
	fresh := progressReceipt("after-answer")
	fresh["createdAt"] = rfc(testNow.Add(time.Minute))
	e.reply("workProgressReceiptsForOwnerRun", old, fresh)
	ok, err = i.ContinueWorkProgress(context.Background(), rc, workstate.Spent{ModelCalls: 24}, 0)
	require.NoError(t, err)
	require.True(t, ok)
	state := rowMap(rowMap(e.callsTo("updateWorkRun")[0].Args(t), "classification"), "progressContinuation")
	require.Equal(t, rfc(testNow), rowString(state, "receiptAfter"))
}
