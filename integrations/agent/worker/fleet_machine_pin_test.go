//go:build agent

package worker

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func pinnedFleetHarness(t *testing.T) (*FleetInference, *modelHop, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	remoteCalls, alternateCalls := &atomic.Int32{}, &atomic.Int32{}
	h := newModelHop(t, func(context.Context, workerservice.ModelCallRequest, func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		remoteCalls.Add(1)
		return workerservice.ModelCallOutcome{Content: "selected machine", FinishReason: workerservice.ModelFinishStop}
	}, nil)
	f := newFleetInference(t, h.store)
	f.selfNodeId, f.forward = nodeA, h.link.router
	alternate := modelMachine("aaa-alternate", map[string]ModelAttributes{hopModel: {}})
	alternate.ConnectedNodeId = nodeA
	h.store.machines = append([]Candidate{alternate}, h.store.machines...)
	w := &workerservice.Worker{RegistrationId: alternate.RegistrationId, OwnerUserId: h.owner, Capabilities: []string{workerservice.ModelCapability}, Labels: alternate.Labels}
	w.SetModelCallFunc(func(_ context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		alternateCalls.Add(1)
		handle, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		finish(workerservice.ModelCallOutcome{Content: "wrong machine", FinishReason: workerservice.ModelFinishStop})
		return handle, nil
	})
	f.registry.Add(w)
	return f, h, remoteCalls, alternateCalls
}

func TestPinnedFleetCallReachesOnlySelectedMachineAcrossReplicaHop(t *testing.T) {
	f, h, remoteCalls, alternateCalls := pinnedFleetHarness(t)
	req := memqlengine.FleetCallRequest{ActingUserId: h.owner, RegistrationId: "v1:worker:registration:laptop", ModelId: hopModel, Kind: memqlengine.FleetKindChat}
	res, err := f.Call(authorityCtx(t, h.owner), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExecutionSurface != "fleet:laptop" || res.Content != "selected machine" || remoteCalls.Load() != 1 || alternateCalls.Load() != 0 {
		t.Fatalf("selected machine proof answered elsewhere: result=%+v remote=%d alternate=%d", res, remoteCalls.Load(), alternateCalls.Load())
	}
}

func TestPinnedFleetRefusesWithoutDispatchingAnAlternate(t *testing.T) {
	for _, why := range []string{"offline", "revoked", "missing model", "context too small", "policy excluded", "not owned", "system caller", "shared foreign machine", "disconnected before start"} {
		t.Run(why, func(t *testing.T) {
			f, h, remoteCalls, alternateCalls := pinnedFleetHarness(t)
			req := memqlengine.FleetCallRequest{ActingUserId: h.owner, RegistrationId: "laptop", ModelId: hopModel, Kind: memqlengine.FleetKindChat}
			selected := &h.store.machines[1]
			switch why {
			case "offline":
				selected.ConnectedNodeId = ""
				selected.LastSeenAt = fleetNow().Add(-time.Hour)
			case "revoked":
				selected.RevokedAt = fleetNow()
			case "missing model":
				selected.Labels = nil
			case "context too small":
				req.ContextTokens = 32768
				selected.Labels = map[string]string{workerservice.ModelLabel(hopModel): "ctx=8192"}
			case "policy excluded":
				h.store.policy = &Policy{RequireLabels: map[string]string{"allow-test": "yes"}}
				h.store.machines[0].Labels["allow-test"] = "yes"
			case "not owned":
				req.RegistrationId = "someone-elses-machine"
			case "system caller":
				req.ActingUserId = ""
			case "shared foreign machine":
				shared := *selected
				shared.OwnerUserId = "v1:identity:user:bob"
				shared.SharingMode, shared.InferenceServe = workerservice.SharingModeCluster, workerservice.InferenceServeCluster
				h.store.machines = h.store.machines[:1]
				f.router = NewRouter(&sharedFleet{fakeFleet: h.store.fakeFleet, all: []Candidate{shared}}, testLogger(), fleetNow)
			case "disconnected before start":
				h.link.handler.registry.Remove("laptop")
			}
			_, err := f.Call(authorityCtx(t, h.owner), req)
			if !errors.Is(err, memqlengine.ErrFleetUnavailable) {
				t.Fatalf("expected fleet refusal, got %v", err)
			}
			if remoteCalls.Load() != 0 || alternateCalls.Load() != 0 {
				t.Fatalf("refused pin dispatched: selected=%d alternate=%d", remoteCalls.Load(), alternateCalls.Load())
			}
		})
	}
}

func TestUnpinnedFleetRetainsNormalMachineOrder(t *testing.T) {
	f, h, remoteCalls, alternateCalls := pinnedFleetHarness(t)
	res, err := f.Call(authorityCtx(t, h.owner), memqlengine.FleetCallRequest{ActingUserId: h.owner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExecutionSurface != "fleet:aaa-alternate" || alternateCalls.Load() != 1 || remoteCalls.Load() != 0 {
		t.Fatalf("unpinned routing changed: %+v", res)
	}
}

func TestPinnedUnreachableReplicaDoesNotBlameUnrelatedOfflineMachine(t *testing.T) {
	f, h, remoteCalls, alternateCalls := pinnedFleetHarness(t)
	h.store.machines[0].LastSeenAt = fleetNow().Add(-time.Hour)
	h.link.reachable = false
	_, err := f.Call(authorityCtx(t, h.owner), memqlengine.FleetCallRequest{
		ActingUserId: h.owner, RegistrationId: "laptop", ModelId: hopModel, Kind: memqlengine.FleetKindChat,
	})
	var refusal *memqlengine.FleetUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("want unavailable, got %v", err)
	}
	if refusal.Total != 1 || len(refusal.Considered) != 0 || strings.Contains(err.Error(), "aaa-alternate") {
		t.Fatalf("selected-machine failure blames unrelated fleet: %+v", refusal)
	}
	if !strings.Contains(err.Error(), ErrNoPeerForNode.Error()) {
		t.Fatalf("lost selected target's failure: %v", err)
	}
	if remoteCalls.Load() != 0 || alternateCalls.Load() != 0 {
		t.Fatal("an unreachable pinned replica dispatched elsewhere")
	}
}
