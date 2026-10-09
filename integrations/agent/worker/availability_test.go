//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

func TestWorkerAvailabilityUsesSharedFleetWithoutALocalStream(t *testing.T) {
	desktop := machine("remote-desktop", func(c *Candidate) {
		c.ConnectedNodeId = "other-agent-replica"
		c.Capabilities = []string{workerservice.CapabilityHeadless, workerservice.CapabilityComputerUse}
		c.Labels = map[string]string{"display": "true"}
	})
	for _, tc := range []struct {
		name               string
		machines           []Candidate
		policy             *Policy
		status             string
		headless, computer bool
	}{
		{"remote desktop", []Candidate{desktop}, nil, "connected", true, true},
		{"headless only", []Candidate{machine("inference")}, nil, "connected", true, false},
		{"no display", []Candidate{func() Candidate { c := desktop; c.Labels = nil; return c }()}, nil, "connected", true, false},
		{"offline", []Candidate{func() Candidate { c := desktop; c.ConnectedNodeId = ""; return c }()}, nil, "disconnected", false, false},
		{"revoked", []Candidate{func() Candidate { c := desktop; c.RevokedAt = fresh(); return c }()}, nil, "unconfigured", false, false},
		{"policy exclusion", []Candidate{desktop}, &Policy{RequireLabels: map[string]string{"os": "unavailable"}}, "disconnected", false, false},
		{"empty", nil, nil, "unconfigured", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fleet := &fakeFleet{owner: ownerA, machines: tc.machines, policy: tc.policy}
			dispatcher := &Dispatcher{router: newTestRouter(fleet)}
			// This replica has NO streams. A local registry probe loses the
			// remote desktop even though ordinary dispatch can reach it.
			integration := NewIntegration(dispatcher, workerservice.NewRegistry(nil, fleetNow), nil, nil)
			nodes, err := integration.handleStatus(asPerson(ownerA), map[string]any{}, 0)
			if err != nil || len(nodes) != 1 {
				t.Fatalf("status: %v %v", nodes, err)
			}
			var got Availability
			if err := json.Unmarshal(nodes[0].Payload, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status || got.HeadlessOnline != tc.headless || got.ComputerUseOnline != tc.computer || got.Online != (tc.headless || tc.computer) {
				t.Fatalf("unexpected availability: %+v", got)
			}
			if len(fleet.touched) != 0 {
				t.Fatal("status reserved a machine or changed routing")
			}
		})
	}
}

func TestWorkerAvailabilityReadFailureIsNotAnOfflineAnswer(t *testing.T) {
	errRead := errors.New("fleet read failed")
	router := newTestRouter(&fakeFleet{readErr: errRead})
	if _, err := router.Availability(context.Background(), ownerA); !errors.Is(err, errRead) {
		t.Fatalf("lost failure: %v", err)
	}
}
