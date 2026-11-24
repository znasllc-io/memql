//go:build agent

package worker

import (
	"context"
	"reflect"
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
