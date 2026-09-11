//go:build agent || planner

package worker

// Selecting a machine for a MODEL call (epic memql#4676, task memql#4678).
//
// There is no second selector here. Models ride the registration/heartbeat
// capability mechanism the way local apps do (`app:<id>`, epic memql#4358):
// a machine advertises `model:<modelId>` in its labels, and selecting one is
// the EXISTING router asked for that label, ordered by the EXISTING four
// strategies. Anything else would be a second thing that disagrees with the
// first, and the disagreement would present as a machine that is eligible on
// the Fleet page and unreachable from a turn.
//
// TWO PROPERTIES ARE SECURITY-LOAD-BEARING, and both are structural rather
// than checked:
//
//   - A MODEL CALL CARRIES THE ACTING USER'S PROMPTS. So it routes ONLY to
//     that user's machines, and the way that is guaranteed is that the
//     user-scoped path reads through WorkersForOwner, which is caller-scoped
//     at the query (`ownerUserId==actor.userId`). There is no filter to forget
//     because there is no cross-user read on this path at all.
//   - A SYSTEM CALL HAS NO ACTING USER, so it cannot use that path. It reaches
//     only machines whose OWNER opted in, and the opt-in is read from
//     `operatorLabels` ALONE -- never from the merge. That distinction is the
//     whole of design D3: `labels` is overwritten from the Register message on
//     every reconnect, so an opt-in stored there would be granted by the
//     machine rather than by its owner, and revoked roughly whenever the lid
//     closed.
//
// CAPABILITY GATING IS AVAILABILITY, NOT AN ERROR. "No machine offers this
// model" and "no machine offering it can do structured output" are the same
// answer to the caller -- the provider is UNAVAILABLE -- because the second is
// not a thing a user can act on differently from the first.

import (
	"context"
	"fmt"
	"strings"

	workerservice "github.com/znasllc-io/memql/component/worker"
	fleetcatalog "github.com/znasllc-io/memql/component/worker/fleetcatalog"
)

// These aliases keep routing and catalog capability parsing on one definition.
const maxQuantLen = fleetcatalog.MaxQuantLen

type ModelNeeds = fleetcatalog.ModelNeeds
type ModelAttributes = fleetcatalog.ModelAttributes

func ParseModelAttributes(value string) ModelAttributes {
	return fleetcatalog.ParseModelAttributes(value)
}
func NeedsForKind(kind string) ModelNeeds { return fleetcatalog.NeedsForKind(kind) }

// PlanModel orders the ACTING USER'S machines that can serve this model.
//
// The ownership boundary is not a filter in this function -- it is the shape
// of the read. WorkersForOwner resolves through a caller-scoped query, so a
// machine belonging to anyone else is not in the result to be filtered out.
func (r *Router) PlanModel(
	ctx context.Context,
	actingUserId string,
	modelId string,
	needs ModelNeeds,
) (RoutePlan, error) {
	if r == nil || r.store == nil {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: no fleet store configured")
	}
	if strings.TrimSpace(actingUserId) == "" {
		// Refused rather than widened. A blank acting user on this path is a
		// caller that failed to resolve one, and quietly treating it as a
		// system call would send somebody's prompt to a machine chosen under
		// a different consent model.
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: a model call needs an acting user; use PlanSharedModel for system work")
	}
	if strings.TrimSpace(modelId) == "" {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: modelId is required")
	}

	plan, err := r.Plan(ctx, actingUserId, workerservice.ModelCapability, nil, nil)
	if err != nil {
		return plan, err
	}
	return narrowToModel(plan, modelId, needs), nil
}

// PlanSharedModel orders the machines eligible for CLUSTER work -- the calls
// with no acting user: system automations, cluster maintenance.
//
// Eligibility here is one extra thing on top of everything PlanModel checks:
// the owner set `sharedInference=true` on the machine's operatorLabels. A
// machine that never opted in is reported as ruled out with that reason, so an
// operator wondering why their fleet is idle for system work reads the answer
// rather than inferring it.
func (r *Router) PlanSharedModel(
	ctx context.Context,
	modelId string,
	needs ModelNeeds,
) (RoutePlan, error) {
	if r == nil || r.store == nil {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: no fleet store configured")
	}
	if strings.TrimSpace(modelId) == "" {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: modelId is required")
	}
	shared, ok := r.store.(SharedFleetStore)
	if !ok {
		return RoutePlan{Policy: DefaultPolicy()},
			fmt.Errorf("worker router: this node's fleet store cannot read shared-inference machines")
	}

	all, err := shared.SharedInferenceWorkers(ctx)
	if err != nil {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: read shared fleet: %w", err)
	}

	// System work runs under the DEFAULT policy, deliberately. A routing
	// policy belongs to a user and expresses how THEY want their machines
	// used; applying one user's policy to a call that may land on another
	// user's machine would let a preference travel across an ownership
	// boundary the rest of this file exists to hold.
	policy := DefaultPolicy()
	kept := make([]Candidate, 0, len(all))
	rejected := map[string]string{}
	for _, c := range all {
		switch {
		case !c.ServesCluster():
			// NAMED, not "not shared". The owner's repair is an act on the
			// Fleet page and the cockpit's is a line in a file on that
			// machine's own disk; one sentence for both sends half the
			// operators to the wrong machine.
			rejected[c.RegistrationId] = c.SharingRefusal()
		case !c.RevokedAt.IsZero():
			rejected[c.RegistrationId] = "revoked"
		case !workerservice.StreamHeld(c.ConnectedNodeId, c.RevokedAt):
			rejected[c.RegistrationId] = "offline"
		case !c.SupportsCapability(workerservice.ModelCapability):
			rejected[c.RegistrationId] = "missing capability " + workerservice.ModelCapability
		default:
			kept = append(kept, c)
		}
	}
	plan := RoutePlan{Policy: policy, Candidates: kept, Rejected: rejected, Total: len(all)}
	return narrowToModel(plan, modelId, needs), nil
}

// narrowToModel filters a capability-level plan down to the machines offering
// this model with the capabilities this prompt needs, then re-orders under the
// policy with the PER-MODEL concurrency as the cap.
//
// The re-order is why this is a second pass rather than a `require` label on
// the first: `leastLoaded` must ration by the ceiling the machine declared for
// THIS MODEL, not by its MODEL-capability slot count. A machine advertising
// max=1 for a 70B model and max=8 for a 1B one has one load ratio per model,
// and ordering by a single number would send eight concurrent 70B calls to a
// laptop that said it could take one.
func narrowToModel(plan RoutePlan, modelId string, needs ModelNeeds) RoutePlan {
	kept := make([]Candidate, 0, len(plan.Candidates))
	if plan.Rejected == nil {
		plan.Rejected = map[string]string{}
	}
	for _, c := range plan.Candidates {
		attrs, offered := c.ModelAttributesFor(modelId)
		if !offered {
			plan.Rejected[c.RegistrationId] = "does not offer model " + modelId
			continue
		}
		if ok, why := attrs.Satisfies(needs); !ok {
			plan.Rejected[c.RegistrationId] = why
			continue
		}
		// The per-model ceiling, stamped onto the MODEL capability slot so
		// the untouched orderCandidates rations by the right number.
		effective := c
		effective.Concurrency = cloneConcurrency(c.Concurrency)
		effective.Concurrency[workerservice.ModelCapability] = attrs.MaxConcurrent
		kept = append(kept, effective)
	}
	orderCandidates(kept, plan.Policy, plan.Prefer, workerservice.ModelCapability)
	plan.Candidates = kept
	return plan
}

func cloneConcurrency(in map[string]uint32) map[string]uint32 {
	out := make(map[string]uint32, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SharedFleetStore is the cluster-wide read the system-call path needs. It is
// a SEPARATE interface from FleetStore, deliberately: every other caller in
// this package is user-scoped, and widening FleetStore would put a cross-owner
// read within reach of code that has no business making one.
type SharedFleetStore interface {
	// SharedInferenceWorkers returns every machine in the cluster with
	// `SharedInference` resolved from its operatorLabels. The FILTERING is
	// the router's -- the store returns them all with the flag projected --
	// so the reason a machine was ruled out can be reported rather than the
	// machine simply being absent.
	SharedInferenceWorkers(ctx context.Context) ([]Candidate, error)
}
