//go:build agent

package worker

import (
	"context"
	"log/slog"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestFleetInferenceWrongPodIsNotLocalWhenStreamElsewhere(t *testing.T) {
	fleet := &FleetInference{
		registry:   workerservice.NewRegistry(slog.Default(), fleetNow),
		selfNodeId: "agent-pgdv6",
		logger:     slog.Default(),
		clock:      fleetNow,
	}
	laptop := Candidate{RegistrationId: "black", ConnectedNodeId: "agent-ddwcc"}
	if fleet.isLocal(laptop) {
		t.Fatal("Ask on sibling must not treat connectedNodeId=ddwcc as local")
	}
}

func TestFleetInferenceRegistryPresenceBeatsStaleConnectedNodeId(t *testing.T) {
	reg := workerservice.NewRegistry(slog.Default(), fleetNow)
	liveMachine(t, reg, "black", 4, nil)
	fleet := &FleetInference{
		registry:   reg,
		selfNodeId: "agent-pgdv6",
		logger:     slog.Default(),
		clock:      time.Now,
	}
	cand := Candidate{RegistrationId: "black", ConnectedNodeId: "agent-ddwcc"}
	if !fleet.isLocal(cand) {
		t.Fatal("holding the stream locally must win over a lagging connectedNodeId")
	}
}

func TestFleetInferenceEmptySelfWithoutRegistryIsNotLocal(t *testing.T) {
	fleet := &FleetInference{
		registry:   workerservice.NewRegistry(slog.Default(), fleetNow),
		selfNodeId: "",
		logger:     slog.Default(),
		clock:      time.Now,
	}
	cand := Candidate{RegistrationId: "black", ConnectedNodeId: "agent-ddwcc"}
	if fleet.isLocal(cand) {
		t.Fatal("empty selfNodeId must not treat unheld machines as local")
	}
}

func TestFleetInferenceEmptyDisconnectIsRefusedBeforeStart(t *testing.T) {
	// When the local holder dies before any delta, Call must see refuse-before-start
	// so another candidate / rebind can run — not a terminal ForwardCompleted.
	reg := workerservice.NewRegistry(slog.Default(), fleetNow)
	liveMachine(t, reg, "black", 4, nil)
	fleet := &FleetInference{
		registry:   reg,
		selfNodeId: "agent-dying",
		logger:     slog.Default(),
		clock:      time.Now,
	}
	// No store/forward: rebind returns nil; attemptLocal path still needs a worker.
	// Simulate by removing the worker after isLocal would have been true — covered
	// by registry miss → ForwardRefusedBeforeStart in attemptLocal.
	reg.Remove("black")
	cand := Candidate{RegistrationId: "black", ConnectedNodeId: "agent-dying", OwnerUserId: "owner"}
	res, outcome, err := fleet.attemptLocal(context.Background(), memqlengine.FleetCallRequest{ActingUserId: "owner", ModelId: "m"}, cand, &memqlv1.ModelCallStart{Model: "m"})
	_ = res
	if outcome != ForwardRefusedBeforeStart {
		t.Fatalf("outcome = %v, want ForwardRefusedBeforeStart after empty holder miss; err=%v", outcome, err)
	}
}
