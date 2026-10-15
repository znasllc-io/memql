//go:build agent

package worker

import (
	"context"
	"fmt"

	workerservice "github.com/znasllc-io/memql/component/worker"
	permission "github.com/znasllc-io/memql/core/computerpermission"
)

// Availability is a read-only routing snapshot, not a grant or a reservation.
// Dispatch still checks scope, approval, kill switches and the live stream.
type Availability struct {
	Status            string                `json:"status"`
	Detail            string                `json:"detail"`
	Online            bool                  `json:"online"`
	Configured        bool                  `json:"configured"`
	HeadlessOnline    bool                  `json:"headlessOnline"`
	ComputerUseOnline bool                  `json:"computerUseOnline"`
	Machines          []MachineAvailability `json:"machines"`
}

// MachineAvailability names actual routing choices, including offline workers
// and older workers that can serve inference but cannot honor typed approval.
// The selector is the enrollment identity, never an operator-editable label.
type MachineAvailability struct {
	BoundedFileReadSupported    bool              `json:"boundedFileReadSupported"`
	HomeDirectory               string            `json:"homeDirectory,omitempty"`
	NativePlatform              string            `json:"platform,omitempty"`
	Name                        string            `json:"name"`
	RequireLabels               map[string]string `json:"requireLabels,omitempty"`
	HeadlessOnline              bool              `json:"headlessOnline"`
	ComputerUseOnline           bool              `json:"computerUseOnline"`
	ComputerPermissionSupported bool              `json:"computerPermissionSupported"`
}

// Availability uses the shared fleet and the owner's routing policy, including
// workers held by other replicas. The local stream registry cannot answer it.
func (r *Router) Availability(ctx context.Context, owner string) (Availability, error) {
	out := Availability{Status: "unconfigured", Machines: []MachineAvailability{}}
	indexes := map[string]int{}
	for _, capability := range []string{workerservice.CapabilityHeadless, workerservice.CapabilityComputerUse} {
		var require map[string]string
		if capability == workerservice.CapabilityComputerUse {
			require = map[string]string{"display": "true"}
		}
		plan, err := r.Plan(ctx, owner, capability, require, nil)
		if err != nil {
			return Availability{}, fmt.Errorf("worker availability: %w", err)
		}
		// A revoked registration is not a configured route. Policy conflicts
		// and excluded machines remain unavailable; do not widen the policy.
		configured := plan.Total
		for _, reason := range plan.Rejected {
			if reason == "revoked" {
				configured--
			}
		}
		out.Configured = out.Configured || configured > 0
		for _, candidates := range [][]Candidate{plan.Candidates, plan.OfflineCandidates} {
			for _, candidate := range candidates {
				index, exists := indexes[candidate.RegistrationId]
				if !exists {
					index = len(out.Machines)
					indexes[candidate.RegistrationId] = index
					machine := MachineAvailability{BoundedFileReadSupported: candidate.ActionContracts.Supports(permission.ReadContract, permission.ReadVersion), Name: candidate.Label(), HomeDirectory: candidate.HomeDirectory, NativePlatform: candidate.NativePlatform, ComputerPermissionSupported: candidate.ActionContracts.Supports(permission.Contract, permission.Version)}
					if candidate.MachineId != "" {
						machine.RequireLabels = map[string]string{"machineId": candidate.MachineId}
					}
					out.Machines = append(out.Machines, machine)
				}
				if workerservice.StreamHeld(candidate.ConnectedNodeId, candidate.RevokedAt) {
					if capability == workerservice.CapabilityHeadless {
						out.Machines[index].HeadlessOnline = true
					} else {
						out.Machines[index].ComputerUseOnline = true
					}
				}
			}
		}
		if len(plan.Candidates) == 0 {
			continue
		}
		out.Online = true
		if capability == workerservice.CapabilityComputerUse {
			out.ComputerUseOnline = true
		} else {
			out.HeadlessOnline = true
		}
		// Prefer the desktop's name when both kinds of worker are present.
		out.Detail = plan.Candidates[0].Label()
	}
	if out.Online {
		out.Status = "connected"
	} else if out.Configured {
		out.Status = "disconnected"
	}
	return out, nil
}
