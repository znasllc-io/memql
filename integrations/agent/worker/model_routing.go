//go:build agent

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
	"sort"
	"strconv"
	"strings"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// SharedInferenceLabel is the operator label by which an owner offers their
// machine for the cluster's own system work.
//
// IT MUST BE SET ON operatorLabels. Nothing enforces that at the wire -- the
// cockpit could report a label of this name -- so the enforcement is that
// Candidate.SharedInference is projected from `operatorLabels` alone, at the
// one place a registration row becomes a Candidate, and no code downstream
// reads the merged map for it.
const SharedInferenceLabel = "sharedInference"

// ModelNeeds is what a particular prompt requires of a model. A zero value
// needs nothing beyond the model existing.
type ModelNeeds struct {
	// StructuredOutput is set for conductor / planner / suggest prompts,
	// which parse the answer rather than reading it.
	StructuredOutput bool
	// Embeddings is set for embedding calls.
	Embeddings bool
	// MinContextWindow is the floor in tokens. Zero means no floor.
	MinContextWindow int
}

// ModelAttributes is what a machine advertised about ONE model, carried as the
// value of its `model:<id>` label.
//
// The encoding is a flat `k=v` list rather than JSON because labels are
// `map[string]string` end to end -- the concept, the wire, the Fleet page --
// and a JSON blob inside one of them would be unreadable in every surface that
// renders labels as text.
//
// EVERY CAPABILITY DEFAULTS TO FALSE, and that direction is deliberate. A
// machine that says nothing about structured output is not selected for a
// structured prompt: a model that silently answers prose to a conductor turn
// produces a parse failure three layers away, naming nothing. Absent is
// "not advertised", which is a fact; assuming yes would be a guess that fails
// late.
type ModelAttributes struct {
	// ContextWindow in tokens. Zero means the machine did not say, which
	// does not meet any floor -- for the same fail-closed reason.
	ContextWindow int
	// StructuredOutput reports that the runtime can honour a response
	// schema for this model.
	StructuredOutput bool
	// Embeddings reports that the model produces vectors.
	Embeddings bool
	// MaxConcurrent is the per-model ceiling. Zero means the machine
	// declared none, which the load ordering reads as unlimited -- the
	// convention loadRatio already uses.
	MaxConcurrent uint32
}

// Attribute keys in the label value.
const (
	attrContext    = "ctx"
	attrStructured = "structured"
	attrEmbeddings = "embeddings"
	attrMax        = "max"
)

// ParseModelAttributes reads the value of a `model:<id>` label.
//
// An unparseable value yields the zero attributes rather than an error: the
// machine is still offering the model, and refusing to route to it over a
// malformed number would take a working machine out of the fleet for a
// cosmetic reason. The zero value is fail-closed on every capability, so the
// worst a garbled value can do is make a machine ineligible for the prompts
// that need a capability it never legibly claimed.
func ParseModelAttributes(value string) ModelAttributes {
	var a ModelAttributes
	for _, part := range strings.Split(value, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case attrContext:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				a.ContextWindow = n
			}
		case attrStructured:
			a.StructuredOutput = parseAdvertisedBool(v)
		case attrEmbeddings:
			a.Embeddings = parseAdvertisedBool(v)
		case attrMax:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				a.MaxConcurrent = uint32(n)
			}
		}
	}
	return a
}

// parseAdvertisedBool accepts the spellings a machine might send. It is
// permissive in exactly one direction: anything it does not recognise is
// FALSE, so a novel spelling costs eligibility rather than granting it.
func parseAdvertisedBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

// String renders attributes back to the label value, so the cockpit contract
// and the engine's reading of it have exactly one definition.
func (a ModelAttributes) String() string {
	parts := make([]string, 0, 4)
	if a.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrContext, a.ContextWindow))
	}
	if a.StructuredOutput {
		parts = append(parts, attrStructured+"=1")
	}
	if a.Embeddings {
		parts = append(parts, attrEmbeddings+"=1")
	}
	if a.MaxConcurrent > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrMax, a.MaxConcurrent))
	}
	return strings.Join(parts, ",")
}

// Satisfies reports whether these attributes meet the prompt's needs, and
// names the miss when they do not. The reason is for the refusal report
// (memql#4682), which lists every machine considered and why each was ruled
// out; it is not a distinct error class.
func (a ModelAttributes) Satisfies(n ModelNeeds) (bool, string) {
	if n.StructuredOutput && !a.StructuredOutput {
		return false, "model does not advertise structured output"
	}
	if n.Embeddings && !a.Embeddings {
		return false, "model does not advertise embeddings"
	}
	if n.MinContextWindow > 0 && a.ContextWindow < n.MinContextWindow {
		if a.ContextWindow == 0 {
			return false, fmt.Sprintf("model advertises no context window (floor %d)", n.MinContextWindow)
		}
		return false, fmt.Sprintf("context window %d is under the floor %d", a.ContextWindow, n.MinContextWindow)
	}
	return true, ""
}

// ModelAttributesFor returns what this machine advertised about a model, and
// whether it advertised it at all.
func (c Candidate) ModelAttributesFor(modelId string) (ModelAttributes, bool) {
	v, ok := c.Labels[workerservice.ModelLabel(modelId)]
	if !ok {
		return ModelAttributes{}, false
	}
	return ParseModelAttributes(v), true
}

// ModelsOffered lists the model ids this machine advertises, sorted so a
// catalog built from several machines is stable.
func (c Candidate) ModelsOffered() []string {
	var out []string
	for k, _ := range c.Labels {
		if id, ok := workerservice.ModelIdFromLabel(k); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Runtimes lists the runtime names this machine reports (ollama,
// openai-compatible). Reported for the operator's benefit; it steers no
// selection, because two machines serving the same model through different
// runtimes are interchangeable to a caller.
func (c Candidate) Runtimes() []string {
	var out []string
	for k := range c.Labels {
		if strings.HasPrefix(k, workerservice.RuntimeLabelPrefix) {
			if name := strings.TrimPrefix(k, workerservice.RuntimeLabelPrefix); name != "" {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

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
	now := r.clock()
	kept := make([]Candidate, 0, len(all))
	rejected := map[string]string{}
	for _, c := range all {
		switch {
		case !c.SharedInference:
			rejected[c.RegistrationId] = "owner has not opted this machine in to shared inference"
		case !workerservice.IsOnline(c.LastSeenAt, c.RevokedAt, now):
			if !c.RevokedAt.IsZero() {
				rejected[c.RegistrationId] = "revoked"
			} else {
				rejected[c.RegistrationId] = "offline"
			}
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
