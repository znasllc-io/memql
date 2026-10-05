//go:build agent

package worker

import (
	"context"
	"sync/atomic"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// Fresh registration generates a bare ID; the engine's internal catalog read
// returns a canonical ID. Exercise the real dispatch and replica hop with
// those different representations of the SAME machine, then their inverse
// after a registration has been read back on reconnect.
func TestFleetModelResponseFindsRegisteredStreamAcrossIDForms(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, streamID := range []string{"laptop", "v1:worker:registration:laptop"} {
			name := "local/" + streamID
			if remote {
				name = "remote/" + streamID
			}
			t.Run(name, func(t *testing.T) {
				var calls atomic.Int32
				h := newModelHop(t, func(context.Context, workerservice.ModelCallRequest, func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
					calls.Add(1)
					return workerservice.ModelCallOutcome{Content: "hello", FinishReason: workerservice.ModelFinishStop}
				}, func(c *Candidate, w *workerservice.Worker) {
					w.RegistrationId = streamID
					c.RegistrationId = "v1:worker:registration:laptop"
					if streamID == c.RegistrationId {
						c.RegistrationId = "laptop"
					}
				})
				f := newFleetInference(t, h.store)
				f.selfNodeId, f.registry = nodeB, h.link.handler.registry
				if remote {
					f.selfNodeId, f.registry, f.forward = nodeA, workerservice.NewRegistry(testLogger(), fleetNow), h.link.router
				}
				res, err := f.Call(authorityCtx(t, h.owner), memqlengine.FleetCallRequest{
					ActingUserId: h.owner, RegistrationId: "laptop", ModelId: hopModel, Kind: memqlengine.FleetKindChat,
				})
				if err != nil {
					t.Fatal(err)
				}
				if res.Content != "hello" || calls.Load() != 1 {
					t.Fatalf("response=%+v, calls=%d; expected one call on the selected machine", res, calls.Load())
				}
			})
		}
	}
}
