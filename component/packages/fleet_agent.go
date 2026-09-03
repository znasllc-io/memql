//go:build agent

package packages

import (
	"context"
	"fmt"
	"strings"

	workerservice "github.com/znasllc-io/memql/component/worker"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// fleet_agent.go supplies the real Fleet dispatcher, on the one node type that
// holds worker streams.
//
// BEHIND THE `agent` TAG because integrations/agent/worker is, and
// component/packages compiles into every node type: an unconditional import
// would make the bff unbuildable. The seam it satisfies is declared in
// fleetbuild.go, which every build compiles.

// routerFleet selects a machine through the Fleet router.
type routerFleet struct {
	router *agentworker.Router
}

// NewRouterFleetDispatcher wraps the Fleet router as a build-route dispatcher.
func NewRouterFleetDispatcher(router *agentworker.Router) FleetDispatcher {
	if router == nil {
		return nil
	}
	return &routerFleet{router: router}
}

// SelectMachine asks the router which of the owner's machines can take this
// build.
//
// UNDER THE OWNER, which is the Fleet's own per-user rule: the machines a
// build may reach are the ones belonging to the person whose package it is,
// and the router resolves that scope from the id rather than from the caller's
// actor. A cluster owner deploying somebody's package does not thereby get to
// use their own laptop for it.
func (f *routerFleet) SelectMachine(ctx context.Context, ownerUserId string, requireLabels map[string]string) (string, string, error) {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return "", "this source has no owner, so there are no machines to choose among", nil
	}
	plan, err := f.router.Plan(ctx, owner, workerservice.CapabilityHeadless, requireLabels, nil)
	if err != nil {
		return "", "", err
	}
	if len(plan.Candidates) == 0 {
		// The router's own sentence names every machine it considered and why
		// each was ruled out -- offline, revoked, missing the capability, or
		// labels that do not match. Passed through rather than summarised,
		// because "no machine available" is the least useful half of it.
		return "", describeRejections(plan), nil
	}
	return plan.Candidates[0].RegistrationId, "", nil
}

// describeRejections turns the router's per-machine reasons into one sentence.
func describeRejections(plan agentworker.RoutePlan) string {
	if len(plan.Rejected) == 0 {
		return "You have no machines paired with this cluster yet."
	}
	parts := make([]string, 0, len(plan.Rejected))
	for machine, why := range plan.Rejected {
		parts = append(parts, fmt.Sprintf("%s: %s", machine, why))
	}
	return "Machines considered -- " + strings.Join(parts, "; ") + "."
}
