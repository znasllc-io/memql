package app

import (
	"context"

	"github.com/znasllc-io/memql/component/planner"
	"github.com/znasllc-io/memql/component/worker"
	plannerintegration "github.com/znasllc-io/memql/integrations/planner"
)

// delegation_probe_wiring.go builds the delegation triage on EVERY node
// type (memql#4362).
//
// No build tag, and that is the point. The triage decides whether a Task
// goes to a local app, and worker streams terminate on the AGENT node
// while Tasks are emitted by the PLANNER node, which runs its own binary
// (app/build_planner.go) and holds no registry at all. Wiring this only
// into the agent build would leave every planner in a real cluster
// answering "no machine": delegation would be inert in the deployed
// topology while every single-process test passed.
//
// This file is also where component/worker's StoredPreference becomes
// component/planner's DelegationPreference. The conversion lives here
// rather than in either package because component/worker is the
// protocol and registry layer -- depending on component/planner from
// there would invert the layering and add a module edge nothing else
// wants. app/ already imports both; glue belongs where the wiring is.

// delegationResolver builds the triage resolver for this node.
//
// registry is nil on a node that holds no worker streams, which is the
// normal case here -- the probe then reads registration rows, keyed on
// the `app:` labels the engine derives and persists alongside the
// inventory. Returns nil when the engine is not yet built; a nil
// resolver makes every Task in-process with `delegation_not_enabled` as
// its recorded reason, which is the correct fail-safe.
func (a *App) delegationResolver(registry *worker.Registry) plannerintegration.DelegationResolver {
	if a == nil || a.engine == nil {
		return nil
	}
	probe := worker.NewDelegationProbe(registry, a.engine)
	return &plannerintegration.PolicyDelegationResolver{
		Policies: storedPreferenceReader{probe: probe},
		Probe:    probe,
	}
}

// storedPreferenceReader converts the worker package's local policy
// shape into the planner's.
type storedPreferenceReader struct {
	probe *worker.DelegationProbe
}

func (r storedPreferenceReader) DelegationPreference(ctx context.Context, ownerUserId string) planner.DelegationPreference {
	stored := r.probe.DelegationPreference(ctx, ownerUserId)
	return planner.DelegationPreference{
		Enabled:               stored.Enabled,
		EligibleKinds:         stored.EligibleKinds,
		AppOrder:              stored.AppOrder,
		MaxConcurrentSessions: stored.MaxConcurrentSessions,
	}
}
