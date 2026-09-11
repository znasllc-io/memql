//go:build agent

package worker

import (
	"log/slog"
	"testing"
	"time"

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
