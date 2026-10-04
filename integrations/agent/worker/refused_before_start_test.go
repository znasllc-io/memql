//go:build agent

package worker

// refused_before_start_test.go -- the dispatcher's own verdict that a call
// started on NO machine, surfaced on the Result (epic memql#5478, #5494).
//
// The re-pick loop has always known it: it is the one fact that decides
// whether moving to the next machine is a re-pick or a second execution. A
// caller that needs it -- the pipelines executor reports a step that never
// ran as pipeline_no_machine_for_need, and one lost mid-run as
// pipeline_node_lost -- used to GUESS it from the error code, and a code
// list misses the codes a sibling replica refuses with (owner_mismatch,
// registration_refused, decode_args, rule 0's), which travel through
// verbatim. CLAUDE.md: refused_before_start must never be guessed.

import (
	"context"
	"errors"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// TestTheResultSaysWhetherAnyMachineStartedTheCall pins the flag on every
// path out of Dispatch: true where the dispatcher is certain nothing started
// on any machine, false wherever a machine may have.
func TestTheResultSaysWhetherAnyMachineStartedTheCall(t *testing.T) {
	sibling := func(remote *fakeRemote) Result {
		elsewhere := machine("laptop")
		elsewhere.ConnectedNodeId = "agent-2"
		store := &fakeStore{fakeFleet: &fakeFleet{machines: []Candidate{elsewhere}}}
		d := newTestDispatcher(t, store, workerservice.NewRegistry(testLogger(), fleetNow), "agent-1", remote)
		res, err := d.Dispatch(context.Background(), approvedRequest())
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		return res
	}
	cases := []struct {
		name        string
		run         func(t *testing.T) Result
		wantCode    string
		wantRefused bool
	}{
		{"refused at a gate", func(t *testing.T) Result {
			f := newPipelineFleet(t)
			res, _ := f.d.Dispatch(context.Background(), pipelineRequest()) // no internal origin
			return res
		}, "denied_pipeline_purpose", true},
		{"no machine matches the requirement", func(t *testing.T) Result {
			f := newPipelineFleet(t, machine("laptop")) // no pipelines=allowed
			res, _ := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
			return res
		}, "no_worker_available", true},
		{"the row says this replica holds the machine and it does not", func(t *testing.T) Result {
			f := newPipelineFleet(t)
			f.registry.Remove("ci-box")
			res, _ := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
			return res
		}, "worker_disconnected", true},
		{"the machine refused under its own policy: it was reached", func(t *testing.T) Result {
			f := newPipelineFleet(t)
			f.failure = &memqlv1.Failure{ErrorCode: "denied_by_policy", ErrorMessage: "pipelines.repos does not list o/r"}
			res, _ := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
			return res
		}, "denied_by_policy", false},
		{"the machine answered", func(t *testing.T) Result {
			f := newPipelineFleet(t)
			res, _ := f.d.Dispatch(asPipelineExecutor(), pipelineRequest())
			return res
		}, "", false},
		{"a sibling refused before start, under a code of its own", func(*testing.T) Result {
			return sibling(&fakeRemote{result: Result{OK: false, ErrorCode: "owner_mismatch"}, outcome: ForwardRefusedBeforeStart})
		}, "owner_mismatch", true},
		{"a sibling could not be reached", func(*testing.T) Result {
			return sibling(&fakeRemote{outcome: ForwardRefusedBeforeStart, err: ErrNoPeerForNode})
		}, "worker_unreachable", true},
		{"a sibling's machine answered with a failure", func(*testing.T) Result {
			return sibling(&fakeRemote{result: Result{OK: false, ErrorCode: "timeout"}, outcome: ForwardCompleted})
		}, "timeout", false},
		{"the forward's answer was lost after the envelope left", func(*testing.T) Result {
			return sibling(&fakeRemote{outcome: ForwardCompleted, err: errors.New("stream reset")})
		}, "worker_disconnected", false},
		{"across a real hop, the sibling's own refusal before start", func(t *testing.T) Result {
			h, ran := recordingHop(t)
			h.registry.WorkerById("laptop").Labels = map[string]string{"os": "linux"}
			res, _ := h.dispatch.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
			h.link.wg.Wait()
			if len(*ran) != 0 {
				t.Fatalf("%d dispatch(es) reached the machine", len(*ran))
			}
			return res
		}, "pipelines_not_allowed", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := c.run(t)
			if res.ErrorCode != c.wantCode {
				t.Fatalf("result = %+v, want error code %q", res, c.wantCode)
			}
			if res.RefusedBeforeStart != c.wantRefused {
				t.Fatalf("%s: RefusedBeforeStart = %v, want %v (result %+v)", c.wantCode, res.RefusedBeforeStart, c.wantRefused, res)
			}
		})
	}
}

// TestASiblingsRefusalBeforeStartKeepsTheMachineItNamed: the flag and the
// machine travel together -- a refusal before start still says which machine
// refused, so a caller can say where it looked.
func TestASiblingsRefusalBeforeStartKeepsTheMachineItNamed(t *testing.T) {
	h := unreachableSibling(t)
	res, err := h.dispatch.Dispatch(asPipelineAcrossTheMesh(t, h), ownersPipelineRequest(h))
	if err != nil || !res.RefusedBeforeStart || res.ErrorCode != "worker_unreachable" {
		t.Fatalf("result = %+v err = %v, want worker_unreachable refused before start", res, err)
	}
	if res.WorkerId != "laptop" || res.NodeId != nodeB {
		t.Fatalf("result names %q on %q, want the laptop on %s", res.WorkerId, res.NodeId, nodeB)
	}
}
