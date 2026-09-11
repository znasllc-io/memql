//go:build agent

package worker

import (
	"context"
	"log/slog"
	"strings"
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

func TestFleetInferenceSelfHolderRegistryMissIsRefusedBeforeStart(t *testing.T) {
	// Prod dump: Ask already on holder pszjr, connectedNodeId==self, WorkerById nil.
	// Must refuse-before-start without claiming peer-forward / retry against connectedNodeId.
	fleet := &FleetInference{
		registry:   workerservice.NewRegistry(slog.Default(), fleetNow),
		selfNodeId: "agent-pszjr",
		logger:     slog.Default(),
		clock:      time.Now,
	}
	_, outcome, err := fleet.attemptLocal(
		context.Background(),
		memqlengine.FleetCallRequest{ActingUserId: "owner", ModelId: "qwen3-embedding:0.6b"},
		Candidate{RegistrationId: "c938433d", ConnectedNodeId: "agent-pszjr", OwnerUserId: "owner", Name: "Jose"},
		&memqlv1.ModelCallStart{Model: "qwen3-embedding:0.6b"},
	)
	if outcome != ForwardRefusedBeforeStart {
		t.Fatalf("outcome=%v err=%v", outcome, err)
	}
	if err == nil || !strings.Contains(err.Error(), "holding replica registry miss") {
		t.Fatalf("want registry-miss error, got %v", err)
	}
	if strings.Contains(err.Error(), "retry against connectedNodeId") || strings.Contains(err.Error(), "forward or retry required") {
		t.Fatalf("self-holder miss must not claim forward/retry helps: %v", err)
	}
}
