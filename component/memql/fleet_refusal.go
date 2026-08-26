package memql

// no_local_model_available -- the refusal that makes "no silent cloud spend"
// structural rather than a convention (epic memql#4676, task memql#4682,
// design D2).
//
// THE DECISION. When a policy's primary is a fleet model and no eligible
// machine can serve it, the work does NOT quietly run on a paid API. Cloud
// runs in exactly two ways, and both are decisions somebody made on the
// record:
//
//   1. an operator wrote `@fallback("streamClaudeSonnet")` into the policy, or
//   2. a person consented -- for this one plan, or this one request.
//
// Everything else refuses, and the work PARKS.
//
// WHY THE REFUSAL CARRIES A REPORT. `no_worker_available` learned this the
// hard way: an empty candidate set says only that the router found nothing,
// while the question a person actually has is which of their four machines was
// ruled out for what. So the refusal names every machine considered and why
// each failed -- offline, revoked, does not offer the model, missing a
// capability -- in the same grammar, so an operator reads one vocabulary
// across both surfaces.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// FeedbackReasonNoLocalModel is the Plan.feedbackReason discriminator for a
// plan parked on an unavailable fleet.
const FeedbackReasonNoLocalModel = "no_local_model_available"

// FleetUnavailable is the typed refusal: no eligible machine for a model, WITH
// the machines considered and why each was ruled out.
type FleetUnavailable struct {
	ModelId string
	// Considered maps machine id -> the reason it was ruled out. Present
	// even -- especially -- when it is the only thing present.
	Considered map[string]string
	// Total is how many machines were looked at before any filtering, which
	// separates "you have no machines" from "none of your four matched".
	// Different problems, different fixes.
	Total int
	// LastError is the failure of the last machine actually attempted, when
	// one was.
	LastError string
}

// Code is the stable machine-readable tag. It is the same string as the
// Plan.feedbackReason value on purpose: an operator reading a log line, a
// park card and a plan row should see one word, not three spellings of it.
func (e *FleetUnavailable) Code() string { return FeedbackReasonNoLocalModel }

func (e *FleetUnavailable) Error() string {
	if e == nil {
		return ErrFleetUnavailable.Error()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: no eligible machine for model %s", FeedbackReasonNoLocalModel, e.ModelId)
	if e.Total == 0 {
		b.WriteString(" (no machines are paired)")
	}
	for _, k := range e.consideredKeys() {
		fmt.Fprintf(&b, "; %s: %s", k, e.Considered[k])
	}
	if e.LastError != "" {
		fmt.Fprintf(&b, "; last attempt: %s", e.LastError)
	}
	return b.String()
}

func (e *FleetUnavailable) consideredKeys() []string {
	keys := make([]string, 0, len(e.Considered))
	for k := range e.Considered {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Unwrap makes errors.Is(err, ErrFleetUnavailable) true, so a caller that only
// wants to know "unavailable" does not have to type-assert.
func (e *FleetUnavailable) Unwrap() error { return ErrFleetUnavailable }

// AsMap renders the refusal for a feedbackRequest payload or a card. The
// machine list is a SLICE rather than a map so the order a person reads is the
// order every reader gets.
func (e *FleetUnavailable) AsMap() map[string]any {
	if e == nil {
		return nil
	}
	considered := make([]map[string]any, 0, len(e.Considered))
	for _, k := range e.consideredKeys() {
		considered = append(considered, map[string]any{"machine": k, "reason": e.Considered[k]})
	}
	out := map[string]any{
		"code":             FeedbackReasonNoLocalModel,
		"model":            e.ModelId,
		"machinesTotal":    e.Total,
		"machinesRuledOut": considered,
	}
	if e.LastError != "" {
		out["lastError"] = e.LastError
	}
	return out
}

// FleetUnavailableFrom extracts the refusal from an error chain.
func FleetUnavailableFrom(err error) (*FleetUnavailable, bool) {
	var out *FleetUnavailable
	if errors.As(err, &out) {
		return out, true
	}
	return nil, false
}

// -----------------------------------------------------------------------------
// Consent
// -----------------------------------------------------------------------------

// cloudConsentCtxKey marks a call as having been authorized to reach a paid
// provider when the local one is unavailable.
type cloudConsentCtxKey struct{}

// ContextWithCloudConsent marks ctx as carrying an explicit, human decision to
// use a cloud provider for this work.
//
// IT IS ONE-SHOT BY CONSTRUCTION: it lives on a context, so it reaches exactly
// the calls made under the context the consenting surface built, and cannot
// outlive them. A stored flag would have to be expired by somebody, and the
// somebody is what would eventually be forgotten -- leaving a cluster that
// "asked once" quietly billing forever.
//
// The per-PLAN form is separate and deliberately more durable: a plan the user
// approved cloud for carries it on its own row, because a plan spans many
// calls and re-asking at each one is not consent, it is nagging.
func ContextWithCloudConsent(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, cloudConsentCtxKey{}, true)
}

// CloudConsentFromContext reports whether this call may fall through to a paid
// provider without an authored fallback. Defaults to FALSE for every context
// that was not stamped, which is the direction that cannot spend money by
// omission.
func CloudConsentFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(cloudConsentCtxKey{}).(bool)
	return v
}

// CloudApprovedMetricKey is where a per-PLAN approval lives on
// Plan.metrics, mirroring budgetApproved / specialistApproved.
const CloudApprovedMetricKey = "cloudApproved"

// HasCloudProviderConfigured reports whether the cluster has any paid provider
// that could serve a call at all.
//
// The park card offers "approve cloud" only when this is true. Offering a
// button that cannot work is worse than offering nothing: it converts "your
// machines are asleep" into "you clicked the fix and it did not fix it", and
// the second is much harder to act on.
func (r *ProviderRegistry) HasCloudProviderConfigured() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name, entry := range r.byName {
		if entry == nil || !entry.Available {
			continue
		}
		if _, isFleet := IsFleetReference(name); isFleet {
			continue
		}
		if strings.EqualFold(entry.Config.Type, FleetProviderType) {
			continue
		}
		if _, ok := entry.Client.(AIProvider); ok {
			return true
		}
	}
	return false
}

// FleetRefusal builds the typed refusal for a model the caller could not use,
// by asking the catalog what the fleet currently offers and why each machine
// was ruled out.
//
// It is on the registry rather than in the router because the catalog read is
// the same one selection makes -- a second implementation of "why not" would
// drift from the one that actually decides, and the drift would present as a
// park card explaining a rule-out that did not happen.
func (r *ProviderRegistry) FleetRefusal(ctx context.Context, actingUserId, modelId string) *FleetUnavailable {
	out := &FleetUnavailable{ModelId: modelId, Considered: map[string]string{}}
	if r == nil {
		return out
	}
	r.mu.RLock()
	f := r.fleet
	r.mu.RUnlock()
	if f == nil {
		out.Considered["(this node)"] = "no fleet inference is installed on the node serving this request"
		return out
	}
	models, err := f.Catalog(ctx, actingUserId)
	if err != nil {
		out.LastError = err.Error()
		return out
	}

	seen := map[string]bool{}
	for _, m := range models {
		for _, machine := range m.Machines {
			seen[machine.RegistrationId] = true
		}
	}
	out.Total = len(seen)

	for _, m := range models {
		if m.ModelId != modelId {
			continue
		}
		for _, machine := range m.Machines {
			label := machine.RegistrationId
			switch {
			case !machine.Online:
				out.Considered[label] = "offline"
			case machine.Busy():
				out.Considered[label] = fmt.Sprintf("busy (%d of %d concurrent calls)", machine.ActiveCount, machine.MaxConcurrent)
			default:
				// Online, not busy, offering the model -- so what ruled it
				// out was a CAPABILITY the prompt needed. Named as a
				// capability miss rather than left unexplained: an entry with
				// no reason reads as a bug in the report.
				out.Considered[label] = "offers the model but not the capability this call needs"
			}
		}
		return out
	}

	// The model is not in the catalog at all. Every machine the user has is
	// therefore a machine that does not offer it, and saying so is more useful
	// than an empty list.
	for id := range seen {
		out.Considered[id] = "does not offer model " + modelId
	}
	return out
}

// HasCloudProviderConfigured lifts the registry answer onto the engine, which
// is what the planner's optional cloudProviderReporter interface asks.
func (e *MemQLEngine) HasCloudProviderConfigured() bool {
	if e == nil {
		return false
	}
	return e.Providers().HasCloudProviderConfigured()
}
