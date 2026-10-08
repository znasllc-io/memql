package automations

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type recoveryPolicyExecutor struct {
	recordingJournalExecutor
	err      error
	decision map[string]any
	source   map[string]any
}

func (e *recoveryPolicyExecutor) RunScopedSnapshot(_ context.Context, source map[string]any, contract, entry string, args map[string]any, ops map[string]memql.WorkflowOperation) (any, error) {
	e.source = source
	if contract != work.SpineContract || entry != "workSpineRecovery" || len(ops) != 0 {
		panic("policy gained effects or wrong contract")
	}
	return e.decision, e.err
}

func TestRecoveryPolicyCannotOverrideBudgetOrInventAnAct(t *testing.T) {
	for _, tc := range []struct {
		act                 string
		spent, max, seconds int
		want                work.Act
		ok                  bool
	}{
		{"retry", 0, 3, 45, work.ActRetry, true}, {"retry", 3, 3, 45, work.ActAsk, true},
		{"repair", 3, 3, 45, work.ActAsk, true}, {"replan", 3, 3, 45, work.ActAsk, true},
		{"succeed", 0, 3, 30, "", false}, {"retry", 0, 3, 0, "", false}, {"retry", 0, 3, 3601, "", false},
	} {
		e := &recoveryPolicyExecutor{decision: map[string]any{"act": tc.act, "retrySeconds": tc.seconds}}
		j := newWorkJournal(e, nil)
		source := map[string]any{"version": "opaque-source-from-run"}
		ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "other-replica", Spine: source})
		act, delay, ok := j.recoveryDecision(ctx, work.SymptomTransient, runRetryBudget{spent: tc.spent, max: tc.max, goalId: "g"})
		if ok != tc.ok || act != tc.want || !reflect.DeepEqual(e.source, source) || (ok && delay != time.Duration(tc.seconds)*time.Second) {
			t.Fatalf("%+v: act=%s delay=%v ok=%v", tc, act, delay, ok)
		}
	}
}

func TestPinnedRecoveryNeverFallsBackWhenInterpreterIsMissing(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		e := &recoveryPolicyExecutor{err: memql.ErrScopedSnapshotUnwired}
		j := newWorkJournal(e, nil)
		var source map[string]any
		if pinned {
			source = map[string]any{"version": "frozen"}
		}
		ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "r", Spine: source})
		_, _, ok := j.recoveryDecision(ctx, work.SymptomTransient, runRetryBudget{spent: 0, max: 3})
		if ok == pinned {
			t.Fatalf("pinned=%v: fallback=%v", pinned, ok)
		}
	}
}
