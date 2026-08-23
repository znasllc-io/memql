package planner

import (
	"context"

	componentplanner "github.com/znasllc-io/memql/component/planner"
)

// delegation.go connects the planner's task emission to the
// delegation triage (memql#4362).
//
// WITHOUT THIS FILE the triage is unreachable code: DecideDelegation
// exists, the probe exists, the DSL fields exist, and no Task would ever
// be created with executionSurface=containerExecutor. The whole feature
// would be present and inert -- which is the shape a reviewer cannot
// tell apart from working, because every unit test still passes.

// DelegationResolver decides, for one Task, whether it is handed to a
// local app on the owner's machine. Nil on a loop means "never
// delegate", which is exactly the behaviour before this landed.
type DelegationResolver interface {
	Resolve(ctx context.Context, ownerUserId, kind string) componentplanner.DelegationDecision
}

// PolicyDelegationResolver reads the owner's delegation policy and runs
// the triage against a live probe.
type PolicyDelegationResolver struct {
	Policies DelegationPolicyReader
	Probe    componentplanner.DelegationProbe
}

// DelegationPolicyReader resolves a user's stored preference. Narrow so
// this file does not import the worker package's store.
type DelegationPolicyReader interface {
	DelegationPreference(ctx context.Context, ownerUserId string) componentplanner.DelegationPreference
}

// Resolve implements DelegationResolver.
func (r *PolicyDelegationResolver) Resolve(ctx context.Context, ownerUserId, kind string) componentplanner.DelegationDecision {
	if r == nil || r.Policies == nil {
		return componentplanner.DelegationDecision{
			Reason: componentplanner.DelegationReasonPolicyOff,
		}
	}
	pref := r.Policies.DelegationPreference(ctx, ownerUserId)
	return componentplanner.DecideDelegation(ctx, ownerUserId, kind, pref, r.Probe)
}

// SetDelegationResolver installs the resolver on the loop. Called from
// the planner node's wiring; leaving it unset keeps every Task
// in-process.
func (l *PlannerAgentLoop) SetDelegationResolver(resolver DelegationResolver) {
	if l == nil {
		return
	}
	l.delegation = resolver
}

// decideDelegation runs the triage for one Task, defaulting to
// in-process with a stated reason when no resolver is installed.
//
// The reason is recorded on BOTH branches. A Task that ran in-process
// when the user expected delegation is a support question, and
// "delegation_not_enabled" answers it where an absent field does not.
func (l *PlannerAgentLoop) decideDelegation(ctx context.Context, ownerUserId, kind string) componentplanner.DelegationDecision {
	if l == nil || l.delegation == nil {
		return componentplanner.DelegationDecision{
			Reason: componentplanner.DelegationReasonPolicyOff,
		}
	}
	return l.delegation.Resolve(ctx, ownerUserId, kind)
}

// SetDelegationResolver installs the resolver on the integration's agent
// loop. Called from the node wiring after the engine exists.
//
// Left unset, every Task is created in-process with
// `delegation_not_enabled` recorded as the reason -- the behaviour that
// predates the triage, and a state a reader can tell apart from "the
// user has delegation on and nothing was online".
func (p *PlannerIntegration) SetDelegationResolver(resolver DelegationResolver) {
	if p == nil || p.agentLoop == nil {
		return
	}
	p.agentLoop.SetDelegationResolver(resolver)
}
