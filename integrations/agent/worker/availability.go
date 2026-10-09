//go:build agent

package worker

import (
	"context"
	"fmt"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// Availability is a read-only routing snapshot, not a grant or a reservation.
// Dispatch still checks scope, approval, kill switches and the live stream.
type Availability struct {
	Status            string `json:"status"`
	Detail            string `json:"detail"`
	Online            bool   `json:"online"`
	Configured        bool   `json:"configured"`
	HeadlessOnline    bool   `json:"headlessOnline"`
	ComputerUseOnline bool   `json:"computerUseOnline"`
}

// Availability uses the shared fleet and the owner's routing policy, including
// workers held by other replicas. The local stream registry cannot answer it.
func (r *Router) Availability(ctx context.Context, owner string) (Availability, error) {
	out := Availability{Status: "unconfigured"}
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
		out.Detail = plan.Candidates[0].Name
	}
	if out.Online {
		out.Status = "connected"
	} else if out.Configured {
		out.Status = "disconnected"
	}
	return out, nil
}
