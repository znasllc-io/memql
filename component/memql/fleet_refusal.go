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

// RefusalCodeNoLocalModel is the stable tag for an unavailable fleet.
//
// It was RefusalCodeNoLocalModel, named for `Plan.feedbackReason` -- a
// field on a concept retired in memql#5000. The NAME went with the row; the
// VALUE stayed, because it is what an operator reads in a log line, on the
// park approval and in the refusal message, and three spellings of one
// condition is what a single constant exists to prevent.
const RefusalCodeNoLocalModel = "no_local_model_available"

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
func (e *FleetUnavailable) Code() string { return RefusalCodeNoLocalModel }

func (e *FleetUnavailable) Error() string {
	if e == nil {
		return ErrFleetUnavailable.Error()
	}
	var b strings.Builder
	if e.ModelId == "*" {
		fmt.Fprintf(&b, "%s: no local model can serve this call", RefusalCodeNoLocalModel)
	} else {
		fmt.Fprintf(&b, "%s: no eligible machine for model %s", RefusalCodeNoLocalModel, e.ModelId)
	}
	if e.Total == 0 {
		b.WriteString(" (no machines are paired)")
	}
	for _, k := range e.consideredKeys() {
		if why, ok := e.Considered[k]; ok {
			fmt.Fprintf(&b, "; %s: %s", k, why)
			continue
		}
		// Synthetic summary keys from consideredKeysForDisplay.
		fmt.Fprintf(&b, "; %s", k)
	}
	if msg := displayLastError(e.LastError); msg != "" {
		fmt.Fprintf(&b, "; last attempt: %s", msg)
	}
	return b.String()
}

// displayLastError keeps operator text free of wrong-pod stream-affinity
// internals ("this replica no longer holds a stream for black") when the
// refusal already names ruled-out machines. Prefer the considered map.
func displayLastError(last string) string {
	last = strings.TrimSpace(last)
	if last == "" {
		return ""
	}
	low := strings.ToLower(last)
	if strings.Contains(low, "holding replica registry miss") ||
		strings.Contains(low, "stream not dispatchable") {
		// Ask was already on the named holder; peer-forward cannot fix a local
		// registry hole. Do not tell the operator to "retry against connectedNodeId".
		return "holding replica registry miss; reconnect required"
	}
	if strings.Contains(low, "no longer holds a stream") ||
		strings.Contains(low, "no live worker stream on this replica") ||
		strings.Contains(low, "forward or retry required") {
		return "holding replica missed; retry against connectedNodeId"
	}
	return last
}

func (e *FleetUnavailable) consideredKeys() []string {
	return e.consideredKeysForDisplay(true)
}

// consideredKeysForDisplay orders machine ids for the refusal text and card.
// When omitNoise is true, revoked registrations are dropped from the operator
// surface (they drown Ask after re-pair lineages) and a single summary line is
// appended when any were omitted. The full map stays on Considered for logs.
func (e *FleetUnavailable) consideredKeysForDisplay(omitNoise bool) []string {
	if e == nil {
		return nil
	}
	if !omitNoise {
		keys := make([]string, 0, len(e.Considered))
		for k := range e.Considered {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	actionable := make([]string, 0, len(e.Considered))
	revoked := 0
	foreignPrivate := 0
	for k, why := range e.Considered {
		switch {
		case why == "revoked":
			revoked++
		case isForeignPrivateShareNoise(why):
			// COUNTED, NEVER COLLECTED. The id is another user's row and this
			// function's output reaches a caller; see the note below.
			foreignPrivate++
		default:
			actionable = append(actionable, k)
		}
	}
	sort.Strings(actionable)
	// FOREIGN IDS ARE AGGREGATED IN BOTH BRANCHES (epic memql#5327, design
	// D12), and the branch that used to restore them is what this replaces.
	//
	// It read: "when EVERY machine was a foreign private share refusal, keep
	// those lines -- they are the only repair the operator has". The first
	// half was true and the second was not. A registration id is not a repair:
	// it names a row the caller cannot read, belonging to somebody whose name
	// they do not learn, and the remedy is identical for every one of them.
	// What it WAS is an enumeration primitive -- a list of other users'
	// machine ids handed to anybody whose call ran out of options, which with
	// the sharing ledger open (finding H-3) was the second half of a way to
	// ask who had been using them.
	//
	// So the foreign-only case keeps its SENTENCE and loses its ids. The
	// sentence is the repair, and it is the same sentence whichever machine
	// you are looking at.
	if revoked > 0 {
		actionable = append(actionable, fmt.Sprintf("(%d revoked registration(s) omitted)", revoked))
	}
	if foreignPrivate > 0 {
		actionable = append(actionable, foreignPrivateSummary(foreignPrivate))
	}
	return actionable
}

// foreignPrivateSummary is the one line a caller gets about machines that are
// not theirs.
//
// It COUNTS and it EXPLAINS, and it names nothing. The count is worth having
// -- "there is hardware on this cluster and none of it is open to you" leads
// somewhere, where silence does not -- and the explanation is the whole repair:
// both halves of the consent have to say cluster, and neither half is the
// caller's to set.
func foreignPrivateSummary(n int) string {
	if n == 1 {
		return "(1 machine on this cluster is not shared with you -- its owner would have to offer it, and the machine itself would have to agree)"
	}
	return fmt.Sprintf("(%d machines on this cluster are not shared with you -- each owner would have to offer theirs, and each machine would have to agree)", n)
}

func isForeignPrivateShareNoise(why string) bool {
	why = strings.ToLower(why)
	return strings.Contains(why, "both are needed") ||
		strings.Contains(why, "has not shared") ||
		strings.Contains(why, "policy.yaml") ||
		strings.Contains(why, "inference.serve") ||
		// A machine lent to NAMED people (epic memql#5344): "shared it with
		// specific people, and not with you" and "... not with the cluster's
		// own work". Somebody else's machine either way, so it is counted and
		// never named, exactly as the two-consent sentences are.
		strings.Contains(why, "specific people")
}

// IsForeignShareRefusal reports whether a routing refusal is about a machine
// that is not the caller's to use -- one this package COUNTS rather than names
// (epic memql#5327, D12). Exported for the parity test that holds it to every
// sentence component/worker can write: the two packages cannot import each
// other, so they agree on words, and a new sentence the classifier missed
// would slip back into the named list as an enumeration of other people's
// machines.
func IsForeignShareRefusal(why string) bool { return isForeignPrivateShareNoise(why) }

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
		if why, ok := e.Considered[k]; ok {
			considered = append(considered, map[string]any{"machine": k, "reason": why})
			continue
		}
		considered = append(considered, map[string]any{"machine": k, "reason": "omitted"})
	}
	out := map[string]any{
		"code":             RefusalCodeNoLocalModel,
		"model":            e.ModelId,
		"machinesTotal":    e.Total,
		"machinesRuledOut": considered,
	}
	if msg := displayLastError(e.LastError); msg != "" {
		out["lastError"] = msg
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
		if _, isApp := IsAppReference(name); isApp {
			continue
		}
		if strings.EqualFold(entry.Config.Type, FleetProviderType) ||
			strings.EqualFold(entry.Config.Type, AppProviderType) {
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

	// THE WILDCARD REPORTS ON MODELS, NOT MACHINES (epic memql#5096, D5).
	// `fleet:*` asked for any eligible local model, so "which machine was
	// ruled out" is the wrong question -- the operator wants to know what
	// their fleet is running and why none of it fit. Reporting machines here
	// would name the same laptop three times, once per model it hosts.
	if modelId == "*" {
		out.Total = len(models)
		for _, m := range models {
			if !m.Online() {
				out.Considered[m.ModelId] = "no machine offering it is online"
				continue
			}
			out.Considered[m.ModelId] = "online, but not eligible for what this call needs"
		}
		return out
	}

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
