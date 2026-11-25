//go:build agent

package worker

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// THE HOP (CLAUDE.md, multi-node is the default). A machine's stream lives on
// one agent replica, and its drops were written by whichever replica held it;
// the replica placing the step reads them from the shared store, holds no
// stream for one of the two machines, and must still wait for the steady
// machine rather than start on the unsteady one -- in both orientations, so
// neither "the waited-for machine is remote" nor "the passed-over machine is
// remote" is the only case that works.
func TestPipelineWaitsForASteadyMachineBusyWithWork(t *testing.T) {
	for _, steadyRemote := range []bool{true, false} {
		name := map[bool]string{true: "steady laptop on the sibling, unsteady desktop here", false: "steady desktop here, unsteady laptop on the sibling"}[steadyRemote]
		t.Run(name, func(t *testing.T) {
			h, dispatcher, _ := siblingThenLocal(t)
			h.link.reachable = true
			for i := range h.store.machines {
				h.store.machines[i].DisplayName = "Box " + h.store.machines[i].RegistrationId
			}
			steady, unsteady := "laptop", "desktop"
			if !steadyRemote {
				steady, unsteady = "desktop", "laptop"
			}
			// The drop was recorded by "the other replica": it is only a row.
			h.store.drops = map[string][]workerservice.ConnectionDrop{unsteady: {dropAt(unsteady, fleetNow().Add(-10*time.Minute))}}
			for i := range h.store.machines {
				if h.store.machines[i].RegistrationId == steady {
					h.store.machines[i].ActiveCount = 1 // busy running another step
				}
			}
			unsteadyRan := 0
			workers := map[string]*workerservice.Worker{"laptop": h.registry.WorkerById("laptop"), "desktop": dispatcher.registry.WorkerById("desktop")}
			// The step the steady machine is running holds a slot on the
			// replica holding its stream, as every call there does; that is
			// the count the placing replica reads when it is that replica.
			if err := workers[steady].Acquire(context.Background(), workerservice.CapabilityHeadless); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { workers[steady].Release(workerservice.CapabilityHeadless) })
			workers[steady].SetDispatchFunc(pipelineCapacityReply("pipeline_capacity_busy", nil), func() {})
			workers[unsteady].SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
				unsteadyRan++
				return okResult(d.GetCallId()), nil
			}, func() {})

			req := ownersPipelineRequest(h)
			req.Connection = ConnectionPreference{Window: window, WaitForSteady: true}
			res, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), req)
			if err != nil || res.OK || !res.RefusedBeforeStart || res.ErrorCode != "pipeline_capacity_busy" || res.WorkerId != steady {
				t.Fatalf("result = %+v err = %v, want the steady %s's busy refusal so the step waits for it", res, err, steady)
			}
			if unsteadyRan != 0 {
				t.Fatalf("the unsteady %s ran the step %d time(s); it must not start while a steady machine is only busy", unsteady, unsteadyRan)
			}
			if len(res.PassedOver) != 1 || res.PassedOver[0].WorkerId != unsteady || res.PassedOver[0].MachineName != "Box "+unsteady || res.PassedOver[0].Health.Steady() {
				t.Fatalf("passed over = %+v, want the unsteady %s by its owner's name with its drop", res.PassedOver, unsteady)
			}
			routing := h.store.lastInvocation(t).Routing
			if got := routing["passedOver"]; !reflect.DeepEqual(got, []string{unsteady}) {
				t.Fatalf("routing passedOver = %v, want [%s]", got, unsteady)
			}
		})
	}
}

// The work that makes a steady machine worth waiting for is usually seconds
// old: several fleet lanes of a run start together, the first takes the steady
// machine's slot, and the rest are refused busy before the replica holding that
// machine's stream has flushed its activeCount to the row (a heartbeat batch, up
// to about 30 s). The evidence that cannot lag is the activity row the holding
// replica writes, synchronously, before it sends the call (activity.go). Here
// the steady laptop's stream is on the sibling, its row still says 0, and the
// sibling is really running the first lane.
func TestPipelineWaitsForASteadyMachineRunningWorkTheOtherReplicaAdmitted(t *testing.T) {
	h, dispatcher, _ := siblingThenLocal(t)
	h.link.reachable = true
	for i := range h.store.machines {
		h.store.machines[i].DisplayName = "Box " + h.store.machines[i].RegistrationId
	}
	h.store.drops = map[string][]workerservice.ConnectionDrop{"desktop": {dropAt("desktop", fleetNow().Add(-10*time.Minute))}}

	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h.registry.WorkerById("laptop").SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release // the first lane runs
			return okResult(d.GetCallId()), nil
		}
		return pipelineCapacityReply("pipeline_capacity_busy", nil)(context.Background(), d, nil)
	}, func() {})
	desktopRan := 0
	dispatcher.registry.WorkerById("desktop").SetDispatchFunc(func(_ context.Context, d *memqlv1.ToolDispatch, _ func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
		desktopRan++
		return okResult(d.GetCallId()), nil
	}, func() {})

	req := ownersPipelineRequest(h)
	req.Connection = ConnectionPreference{Window: window, WaitForSteady: true}
	firstDone := make(chan Result, 1)
	go func() {
		res, _ := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), req)
		firstDone <- res
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first lane never reached the steady laptop")
	}
	for i := range h.store.machines {
		if h.store.machines[i].RegistrationId == "laptop" && h.store.machines[i].ActiveCount != 0 {
			t.Fatal("the fixture must leave the row's activeCount stale at 0")
		}
	}

	second, err := dispatcher.Dispatch(asPipelineAcrossTheMesh(t, h), req)
	close(release)
	if first := <-firstDone; !first.OK || first.WorkerId != "laptop" {
		t.Fatalf("first lane = %+v, want it run on the steady laptop", first)
	}
	if err != nil || second.OK || !second.RefusedBeforeStart || second.ErrorCode != "pipeline_capacity_busy" || second.WorkerId != "laptop" {
		t.Fatalf("second lane = %+v err = %v, want the steady laptop's busy refusal so it waits for it", second, err)
	}
	if desktopRan != 0 || len(second.PassedOver) != 1 || second.PassedOver[0].WorkerId != "desktop" {
		t.Fatalf("the unsteady desktop ran %d time(s), passed over = %+v; it must wait for the laptop's running lane", desktopRan, second.PassedOver)
	}
}
