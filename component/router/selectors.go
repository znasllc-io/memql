package router

// selectors.go -- turning one chain ENTRY into the concrete registry names to
// try, in order (epic memql#5127, task memql#5131, design D4).
//
// A POLICY WRITES A KIND, NOT A MODEL. `fleet:strongest` is a question about a
// catalog that changes when a laptop wakes; `federation:cheapest` is a question
// about prices that change when a vendor publishes new ones. Pinning either at
// authoring time is how a chain comes to park on a fleet that would have served
// the turn perfectly, or to spend on a model that stopped being the cheap one.
//
// EXPANSION HAPPENS AT RESOLVE TIME, AND THAT PLACEMENT IS THE POINT. The
// decision record has to name a MODEL -- "fleet:strongest served it" is not an
// answer anybody can audit -- and only the request knows the context-window
// floor, which the call-time chooser inside the fleet provider cannot see.
//
// EVERY CANDIDATE DROPPED LEAVES A LINE. A selector that quietly returned a
// shorter list would turn "your fleet has nothing that can do this" back into
// the sentence memql#4682 exists to replace.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
)

// FederationReferencePrefix is how a policy names the federation door
// explicitly: `federation:cheapest`, `federation:strongest`,
// `federation:<providerName>`.
//
// A BARE PROVIDER NAME MEANS THE SAME THING and stays legal, which is why this
// prefix is a spelling rather than a requirement: `doorFor` classifies both as
// federation, so the ceiling gates both identically.
const FederationReferencePrefix = "federation:"

// EmbedderReferencePrefix is how a policy names the cluster's ACTIVE EMBEDDER
// BINDING (epic memql#5137). The grammar half ships with the rules epic so
// that epic never has to touch the parser; the resolver is its own.
const EmbedderReferencePrefix = "embedder:"

// EmbedderSelectorActive is the only word the embedder door takes.
//
// It is spelled out rather than left implicit (`embedder:` alone) because a
// chain entry that is a bare prefix reads as a typo, and because naming it
// leaves room for nothing: there is no cheapest or strongest embedder to ask
// for, only the one this cluster's index was built with.
const EmbedderSelectorActive = "active"

// Federation selectors -- orderings over the registry's federated records.
const (
	// FederationSelectorCheapest orders by input plus output cost per million,
	// ascending.
	FederationSelectorCheapest = "cheapest"
	// FederationSelectorStrongest orders by capability. There is no capability
	// measure for a vendor record this epic; see federationCandidates.
	FederationSelectorStrongest = "strongest"
)

// PolicyReferencePrefix is the entry form a policy uses to name another
// policy. It must never reach this file: chains are expanded at LOAD.
const PolicyReferencePrefix = "policy:"

// candidate is one concrete registry name the chain walk may try.
type candidate struct {
	// Name is the registry name to look up -- a real provider record, or the
	// `fleet:<modelId>` / `app:<id>` form the registry synthesizes.
	Name string
	// Door is the door this candidate belongs to, classified from the entry
	// the AUTHOR wrote rather than from the expanded name, so an operator
	// reading the decision sees the step they wrote.
	Door string
}

// expandEntry resolves one chain entry into the concrete names to try, in
// order, recording a reason for everything it drops on the way.
//
// It returns an error only for an entry that should have been impossible --
// a `policy:` reference that survived load, or a selector word nobody defined.
// An entry that legitimately resolves to nothing returns an empty list with
// its reason already on the report, because "the fleet had nothing eligible"
// is an answer rather than a fault.
func (r *Router) expandEntry(ctx context.Context, req ResolveRequest, entry string, report *doorReporter) ([]candidate, error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		report.note(entry, "the chain contains an empty entry")
		return nil, nil
	}

	// A POLICY REFERENCE HERE IS A LOADER BUG, NOT AN UNKNOWN PROVIDER, and
	// saying so is the difference between a five-minute fix and an afternoon.
	// Treating it as a provider name would report "no provider by that name is
	// registered" for `policy:localFirst`, which sends a reader to the
	// provider registry to look for something that was never meant to be there.
	if strings.HasPrefix(entry, PolicyReferencePrefix) {
		return nil, fmt.Errorf("router: chain entry %q reached resolution: policy references are expanded at load, "+
			"so a surviving one means the policy registry handed out an unexpanded chain", entry)
	}

	if selector, ok := memql.IsFleetSelector(entry); ok {
		return r.fleetSelectorCandidates(ctx, req, entry, selector, report)
	}
	if _, ok := memql.IsFleetReference(entry); ok {
		// A concrete `fleet:<modelId>`, including the retired `fleet:*`, which
		// the registry entry refuses by name with the replacement spelled out.
		return []candidate{{Name: entry, Door: DoorLocal}}, nil
	}
	if _, ok := memql.IsAppReference(entry); ok {
		// `app:*` and `app:<id>` both resolve through the registry's own app
		// path, which picks among the owner's signed-in machines. There is
		// nothing for a selector to order here that the app door does not
		// already order itself.
		return []candidate{{Name: entry, Door: DoorApp}}, nil
	}

	// THE EMBEDDER BINDING IS NOT A DOOR, so it has neither a fleet nor a
	// federation arm: whichever model is bound may live on either side, and
	// the door is DERIVED from what the binding resolves to.
	//
	// THIS SELECTOR RETURNS EXACTLY ONE CANDIDATE OR NONE, and the absence of
	// an ordering is the whole point. Every other selector here asks a
	// question with several defensible answers and picks the best one; this
	// one asks which embedder the cluster is CURRENTLY WRITING VECTORS WITH,
	// and that has one answer by construction. A second candidate would be a
	// different vector space, and falling through to it would write vectors
	// the index cannot compare -- silently, since every subsequent similarity
	// read returns plausible neighbours that are simply wrong.
	//
	// So an unbound cluster reports the refusal and stops. It does NOT fall
	// through to a fleet or federation selector, because "some embedder" is
	// not a weaker version of the right answer here; it is a corrupted index.
	if strings.HasPrefix(entry, EmbedderReferencePrefix) {
		selector := strings.TrimSpace(strings.TrimPrefix(entry, EmbedderReferencePrefix))
		if selector != EmbedderSelectorActive {
			report.note(entry, fmt.Sprintf(
				"the embedder door takes %q and nothing else; %q names no selector. A cluster has one active "+
					"binding by construction, so there is nothing here to order or choose between",
				EmbedderSelectorActive, selector))
			return nil, nil
		}
		name, err := memql.ResolveEmbedderProvider(ctx)
		if err != nil {
			report.note(entry, err.Error())
			return nil, nil
		}
		return []candidate{{Name: name, Door: doorFor(name)}}, nil
	}

	if strings.HasPrefix(entry, FederationReferencePrefix) {
		name := strings.TrimSpace(strings.TrimPrefix(entry, FederationReferencePrefix))
		switch name {
		case FederationSelectorCheapest, FederationSelectorStrongest:
			return r.federationCandidates(name, report), nil
		case "":
			report.note(entry, "the entry names the federation door with no provider or selector after it")
			return nil, nil
		}
		return []candidate{{Name: name, Door: DoorFederation}}, nil
	}

	// A bare name is a provider record, exactly as it always was.
	return []candidate{{Name: entry, Door: DoorFederation}}, nil
}

// fleetSelectorCandidates asks the live catalog, in the selector's order, for
// the models that can serve THIS call.
//
// The needs come off the REQUEST rather than off the call about to be placed,
// which is what lets the context-window floor apply at all: the call-time
// chooser knows whether a schema and tools are present and knows nothing about
// how long the rendered prompt is.
func (r *Router) fleetSelectorCandidates(
	ctx context.Context,
	req ResolveRequest,
	entry, selector string,
	report *doorReporter,
) ([]candidate, error) {
	if r == nil || r.providers == nil {
		report.note(entry, "no provider registry")
		return nil, nil
	}
	models, err := r.providers.FleetCandidatesFor(ctx, req.UserId, selector, fleetNeedsFor(req))
	if err != nil {
		report.note(entry, err.Error())
		return nil, nil
	}

	out := make([]candidate, 0, len(models))
	ruledOut := map[string]string{}
	for _, m := range models {
		name := memql.FleetReferencePrefix + m.ModelId
		if !m.Eligible {
			ruledOut[m.ModelId] = m.Reason
			// The decision keeps the per-model line; the refusal's door list
			// keeps ONE line for the entry an author wrote, below, with the
			// same detail attached. A door list with a line per catalog model
			// would report a fleet of eight models as eight shut doors.
			report.noteConsidered(name, DoorLocal, m.Reason)
			continue
		}
		out = append(out, candidate{Name: name, Door: DoorLocal})
	}
	if len(out) == 0 {
		report.noteLocal(entry, fleetSelectorReason(selector, len(models)), ruledOut)
	}
	return out, nil
}

// fleetSelectorReason distinguishes a fleet with NO models from one whose
// models did not match. They are different problems with different fixes --
// wake a machine and pull a model, versus pull a bigger or better-equipped one
// -- and a single sentence for both sends a person to the wrong place.
func fleetSelectorReason(selector string, total int) string {
	if total == 0 {
		return "no machine offers any model"
	}
	return fmt.Sprintf("no local model can serve this call (%s of %d considered)", selector, total)
}

// fleetNeedsFor translates the request's capability floors into the fleet
// catalog's own vocabulary.
//
// MinContextTokens crosses into MinContextWindow unchanged, and that is the
// wire this epic connects: FleetCallRequest.Needs() derives its floor from the
// call and has always left it zero, so the catalog's context gate has never
// once run. It runs now, which means a machine that reports no context window
// for a model is passed over as soon as a call declares a floor.
func fleetNeedsFor(req ResolveRequest) memql.FleetNeeds {
	needs := memql.FleetNeeds{
		Vision:           req.Needs.Vision || req.Modality == airoute.ModalityVision,
		AudioIn:          req.Needs.AudioIn || req.Modality == airoute.ModalityTranscribe,
		AudioOut:         req.Needs.AudioOut || req.Modality == airoute.ModalitySpeech,
		ImageGen:         req.Needs.Image,
		StructuredOutput: req.Needs.Structured,
		// Embeddings is DERIVED from the modality rather than carried as a
		// field on Needs, and adding the field back is the mistake to avoid:
		// a second copy of one fact makes `Modality: embedding, Needs:
		// {Embeddings: false}` expressible, contradictory and silently
		// resolvable -- which routes an embedding call to a chat model.
		// Vision, AudioIn, AudioOut and Image ARE fields, because a caller may
		// assert them independently of the modality (a chat call can require
		// vision). Nothing but an embedding call ever wants an embedder.
		Embeddings:       req.Modality == airoute.ModalityEmbedding,
		Tools:            req.Needs.Tools,
		MinContextWindow: req.Needs.MinContextTokens,
	}
	if req.Modality == airoute.ModalitySpeech || req.Modality == airoute.ModalityTranscribe {
		needs.MinContextWindow = 0
	}
	return needs
}

// federationCandidates orders the registry's federated records.
//
// CHEAPEST is input plus output cost per million, ascending, and A RECORD WITH
// NO COST FIGURES SORTS LAST. Sorting it first is the natural bug and the
// expensive one: an unpriced record reads as zero, zero is the smallest sum in
// the catalog, and the record nobody filled in becomes the cheapest thing the
// router can find -- silently free rather than visibly unpriced. The note says
// which it is.
//
// STRONGEST has NO MEASURE THIS EPIC. There is no capability figure on a
// provider record and no benchmark behind one, so the order is the registry's
// own -- stable, replica-identical, and a PLACEHOLDER rather than a ranking.
// Epic 4 fills it in. A proxy computed from context window or price would rank
// the catalog convincingly and mean nothing, which is worse than an order that
// admits what it is.
func (r *Router) federationCandidates(selector string, report *doorReporter) []candidate {
	if r == nil || r.providers == nil {
		return nil
	}
	type priced struct {
		name    string
		total   float64
		unknown bool
	}
	// Names() is sorted, so the input order every sort below is stable over is
	// the same on every replica.
	names := r.providers.Names()
	records := make([]priced, 0, len(names))
	for _, name := range names {
		entry, ok := r.providers.Entry(name)
		if !ok || entry == nil {
			continue
		}
		p := entry.Config.Pricing()
		records = append(records, priced{
			name:    name,
			total:   p.InputPerMillion + p.OutputPerMillion,
			unknown: !p.Configured(),
		})
	}

	if selector == FederationSelectorCheapest {
		sort.SliceStable(records, func(i, j int) bool {
			a, b := records[i], records[j]
			if a.unknown != b.unknown {
				return !a.unknown
			}
			if a.total != b.total {
				return a.total < b.total
			}
			return a.name < b.name
		})
	}

	out := make([]candidate, 0, len(records))
	for _, rec := range records {
		if selector == FederationSelectorCheapest && rec.unknown {
			report.noteConsidered(rec.name, DoorFederation,
				"declares no inputCostPerMillion or outputCostPerMillion, so it sorts last among federated records "+
					"rather than reading as free")
		}
		out = append(out, candidate{Name: rec.name, Door: DoorFederation})
	}
	return out
}

// contextFloorMiss reports why a registry record cannot hold this call, or ""
// when it can.
//
// AN UNDECLARED CONTEXT WINDOW IS A MISS, not a pass, and the reason names the
// param to add. The alternative reads a missing figure as "big enough", which
// is the same silence-wins failure the fleet ordering was built to avoid,
// arriving at the one place where being wrong means a truncated answer nobody
// can see. The two messages are deliberately different: one fix is a bigger
// model, the other is a line in a provider record.
func contextFloorMiss(cfg memql.ProviderConfig, floor int) string {
	if floor <= 0 {
		return ""
	}
	window := cfg.ContextWindow()
	if window <= 0 {
		return fmt.Sprintf("declares no contextWindow param, so it cannot be shown to hold this call's %d-token floor", floor)
	}
	if window < floor {
		return fmt.Sprintf("context window %d is under the floor %d", window, floor)
	}
	return ""
}
