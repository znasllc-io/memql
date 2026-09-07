package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/core/common"
)

type recordingRecorder struct {
	mu   sync.Mutex
	seen []ToolInvocation
	err  error
}

func (r *recordingRecorder) RecordToolInvocation(_ context.Context, inv ToolInvocation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, inv)
	return r.err
}

func (r *recordingRecorder) all() []ToolInvocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ToolInvocation(nil), r.seen...)
}

// countingExecutor answers every dispatch and counts them.
type countingExecutor struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (e *countingExecutor) ExecuteToolByName(_ context.Context, name string, _ map[string]any) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return "result of " + name, e.err
}

func runCtx(runId, owner string) context.Context {
	return common.ContextWithRun(context.Background(), common.RunContext{
		RunId:       runId,
		StepKey:     "step-a",
		OwnerUserId: owner,
		Mode:        common.RunModeLive,
	})
}

// TestATurnWithNToolCallsProducesNRecords is memql#5050's acceptance
// criterion, stated directly.
func TestATurnWithNToolCallsProducesNRecords(t *testing.T) {
	exec := &countingExecutor{}
	rec := &recordingRecorder{}
	r := newToolRecorder(exec, nil)
	r.SetRecorder(rec)

	ctx := runCtx("run-1", "user-1")
	const n = 5
	for i := 0; i < n; i++ {
		if _, err := r.ExecuteToolByName(ctx, "workbenchHost", map[string]any{"i": i}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	got := rec.all()
	if len(got) != n {
		t.Fatalf("recorded %d invocations for %d tool calls", len(got), n)
	}
	if exec.calls != n {
		t.Fatalf("dispatched %d tools for %d calls", exec.calls, n)
	}
	// Ordering is what the transcript reproduces, so seq must be dense and
	// monotonic rather than merely present.
	for i, inv := range got {
		if inv.Seq != i {
			t.Errorf("call %d recorded seq %d; a transcript sorted on seq would reorder the run", i, inv.Seq)
		}
		if inv.RunId != "run-1" || inv.OwnerUserId != "user-1" || inv.StepKey != "step-a" {
			t.Errorf("call %d lost its run context: %+v", i, inv)
		}
	}
}

// TestSeqIsPerRunNotPerProcess: two runs interleaving through one replier must
// each get their own dense ordering, or one run's transcript is shuffled by
// the other run's traffic.
func TestSeqIsPerRunNotPerProcess(t *testing.T) {
	rec := &recordingRecorder{}
	r := newToolRecorder(&countingExecutor{}, nil)
	r.SetRecorder(rec)

	a, b := runCtx("run-A", "u"), runCtx("run-B", "u")
	for _, ctx := range []context.Context{a, b, a, b, a} {
		if _, err := r.ExecuteToolByName(ctx, "t", nil); err != nil {
			t.Fatal(err)
		}
	}

	perRun := map[string][]int{}
	for _, inv := range rec.all() {
		perRun[inv.RunId] = append(perRun[inv.RunId], inv.Seq)
	}
	for runId, seqs := range perRun {
		for i, s := range seqs {
			if s != i {
				t.Errorf("%s got seqs %v, want dense 0..n-1", runId, seqs)
				break
			}
		}
	}
}

// TestNoRunMeansNoRecord is the deliberate behaviour change from taskstamp,
// which minted a synthetic ad-hoc Plan so every chat-driven tool call had a
// parent. Minting a RUN would be worse than the Plan was: a run row names an
// automation and would be claimed and executed by the run dispatcher.
func TestNoRunMeansNoRecord(t *testing.T) {
	exec := &countingExecutor{}
	rec := &recordingRecorder{}
	r := newToolRecorder(exec, nil)
	r.SetRecorder(rec)

	if _, err := r.ExecuteToolByName(context.Background(), "workbenchHost", nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if exec.calls != 1 {
		t.Fatalf("the tool must still run outside a run; calls=%d", exec.calls)
	}
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("recorded %d invocations for a turn with no run", len(got))
	}
}

// TestAFailedToolIsStillRecorded: the record is of what HAPPENED, and a failed
// call happened. The transcript excludes it on read (isError), which is a
// different decision made in a different place.
func TestAFailedToolIsStillRecorded(t *testing.T) {
	boom := errors.New("tool exploded")
	rec := &recordingRecorder{}
	r := newToolRecorder(&countingExecutor{err: boom}, nil)
	r.SetRecorder(rec)

	if _, err := r.ExecuteToolByName(runCtx("run-1", "u"), "brokenTool", nil); !errors.Is(err, boom) {
		t.Fatalf("the tool's error must reach the caller unchanged, got %v", err)
	}
	got := rec.all()
	if len(got) != 1 || !got[0].IsError || got[0].Error != boom.Error() {
		t.Fatalf("a failed call must be recorded as failed: %+v", got)
	}
}

// TestARecordingFailureNeverFailsTheToolCall keeps taskstamp's one genuinely
// good property: bookkeeping must not be able to break the product.
func TestARecordingFailureNeverFailsTheToolCall(t *testing.T) {
	rec := &recordingRecorder{err: errors.New("database is on fire")}
	r := newToolRecorder(&countingExecutor{}, nil)
	r.SetRecorder(rec)

	out, err := r.ExecuteToolByName(runCtx("run-1", "u"), "workbenchHost", nil)
	if err != nil {
		t.Fatalf("a recording failure broke the tool call: %v", err)
	}
	if out != "result of workbenchHost" {
		t.Fatalf("the tool's result was altered: %q", out)
	}
}

// TestNoRecorderStillDispatches: a node with no work integration runs tools
// exactly as before.
func TestNoRecorderStillDispatches(t *testing.T) {
	exec := &countingExecutor{}
	r := newToolRecorder(exec, nil)
	// deliberately no SetRecorder

	if _, err := r.ExecuteToolByName(runCtx("run-1", "u"), "workbenchHost", nil); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if exec.calls != 1 {
		t.Fatalf("calls=%d, want 1", exec.calls)
	}
}
