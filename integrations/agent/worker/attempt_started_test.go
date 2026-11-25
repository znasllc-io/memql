//go:build agent

package worker

import (
	"context"
	"reflect"
	"sync"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// WHERE A CALL STARTED, BEFORE IT ENDS (Request.OnStarted). An attempt's first
// output is the proof the call started on that machine -- a refusal before
// start sends none -- so the dispatcher says where then, once, before that
// output reaches the caller. The pipeline executor uses it to put "the step was
// placed on an unsteady machine" above the step's own lines instead of below
// them.
func TestTheFirstOutputSaysWhereTheStepStarted(t *testing.T) {
	f := connectionFleet(t, "desktop", "laptop")
	// The steady desktop refuses before start and sends nothing, so it is never
	// said to have started; the unsteady laptop runs the step.
	f.registry.WorkerById("desktop").SetDispatchFunc(pipelineCapacityReply("pipeline_capacity_fenced", nil), func() {})
	f.registry.WorkerById("laptop").SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, onChunk func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		for _, line := range []string{"cloning\n", "testing\n"} {
			onChunk(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_StdoutChunk{StdoutChunk: []byte(line)}})
		}
		return okResult(d.GetCallId()), nil
	}, func() {})

	var order []string
	var started []AttemptStarted
	req := connectionRequest(true)
	req.OnStarted = func(s AttemptStarted) {
		started = append(started, s)
		order = append(order, "started on "+s.MachineName)
	}
	req.OnStreamChunk = func(c *nodev1.WorkerForwardStream) { order = append(order, "output "+string(c.GetStdoutChunk())) }
	res, err := f.d.Dispatch(asPipelineExecutor(), req)
	if err != nil || !res.OK || res.WorkerId != "laptop" {
		t.Fatalf("result = %+v err = %v, want the step on the laptop", res, err)
	}
	if want := []string{"started on Box laptop", "output cloning\n", "output testing\n"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %q, want %q: said once, before the first output", order, want)
	}
	s := started[0]
	if s.WorkerId != "laptop" || s.Connection == nil || s.Connection.Steady() || s.Connection.Drops != 1 {
		t.Fatalf("started = %+v, want the laptop with its one drop", s)
	}
}

// The same across the hop (CLAUDE.md, multi-node is the default): the machine's
// stream is on the sibling, its output reaches this replica over the forward,
// and the first of it still says where the call started, once and first.
func TestTheFirstOutputAcrossTheHopSaysWhereTheCallStarted(t *testing.T) {
	h := newHop(t, func(_ context.Context, d *memqlv1.ToolDispatch, onChunk func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		onChunk(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_StdoutChunk{StdoutChunk: []byte("one")}})
		onChunk(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_StderrChunk{StderrChunk: []byte("two")}})
		return okResult(d.GetCallId()), nil
	}, nil)

	var mu sync.Mutex
	var order []string
	req := h.request()
	req.OnStarted = func(s AttemptStarted) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "started on "+s.WorkerId)
	}
	req.OnStreamChunk = func(c *nodev1.WorkerForwardStream) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "chunk")
	}
	if _, err := h.dispatch.Dispatch(authorityCtx(t, h.owner), req); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	h.link.wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"started on laptop", "chunk", "chunk"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %q, want %q", order, want)
	}
}
