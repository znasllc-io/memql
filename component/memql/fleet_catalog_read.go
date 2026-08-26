package memql

// The `fleetModels` and `inferenceStatus` VIRTUAL READS (epic memql#4676,
// tasks memql#4683 and memql#4684).
//
// ONE SOURCE, THREE READERS. The router selects on this catalog, the portal's
// Providers page renders it, and the first-run gate decides eligibility from
// it. A second implementation of "which models can this cluster actually use"
// would drift from the one that decides, and the drift presents in the worst
// possible way: a page saying a model is available while every call to it
// parks, or a gate letting a user through to a console whose features all
// refuse.
//
// NEVER PERSISTED, the `v1:router:modelCatalog` pattern -- and here the reason
// is at its sharpest. The answer is which machines are awake RIGHT NOW, so a
// stored copy could only be a second, staler answer, and its staleness would
// be indistinguishable from the very condition it describes: a row written
// four minutes ago saying "llama3.1:8b is available" describes a closed laptop
// exactly as confidently as an open one.
//
// SCOPED TO THE CALLER, and that is not a convenience. A model call carries
// the caller's prompts and routes only to their machines (memql#4678); a
// catalog showing somebody else's would promise a model the router will never
// use, and would also enumerate another user's hardware. So this read answers
// for the CALLER'S OWN machines, plus the shared-inference set every user may
// legitimately reach for cluster work.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// FleetModelConcept is the canonical id of the catalog projection.
const FleetModelConcept = "v1:platform:fleetModel"

// InferenceStatusConcept is the canonical id of the eligibility projection.
const InferenceStatusConcept = "v1:platform:inferenceStatus"

// The three doors from design G, as the status row names them.
const (
	InferenceDoorLocal      = "local"
	InferenceDoorFederation = "federation"
	InferenceDoorApiKey     = "apiKey"
)

// MinimumContextWindow is the default context floor a model must advertise to
// count toward first-run eligibility.
//
// It is a FLOOR FOR THE GATE, not a per-call rule: what enforces capability
// per call is the catalog gating the router applies (memql#4679). 8k is
// deliberately low -- it is the smallest window in which the platform's own
// structured prompts fit at all, so a machine over it is usable while a
// machine under it would fail every conductor turn with a truncation nobody
// could read as a cause.
const MinimumContextWindow = 8192

// evaluateFleetModelsExpression produces one row per model the caller's fleet
// offers, sorted so a client rendering a list gets a stable order.
func (e *MemQLEngine) evaluateFleetModelsExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if e == nil || e.providers == nil {
		return nil, nil
	}
	models, err := e.fleetCatalogForCaller(ctx)
	if err != nil {
		return nil, err
	}

	nodes := make([]memorynodes.MemoryNode, 0, len(models))
	for _, m := range models {
		machines := make([]any, 0, len(m.Machines))
		online := 0
		for _, mm := range m.Machines {
			if mm.Online {
				online++
			}
			machines = append(machines, map[string]any{
				"registrationId": mm.RegistrationId,
				"name":           mm.Name,
				"displayName":    mm.DisplayName,
				"runtimes":       toAnySlice(mm.Runtimes),
				"online":         mm.Online,
				"busy":           mm.Busy(),
				"activeCount":    mm.ActiveCount,
				"maxConcurrent":  int(mm.MaxConcurrent),
			})
		}
		raw, err := json.Marshal(map[string]any{
			"modelId":          m.ModelId,
			"contextWindow":    m.ContextWindow,
			"structuredOutput": m.StructuredOutput,
			"embeddings":       m.Embeddings,
			"online":           m.Online(),
			"machineCount":     len(m.Machines),
			"onlineCount":      online,
			"machines":         machines,
		})
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, memorynodes.MemoryNode{
			// The row id IS the model id: there is exactly one row per model
			// and the model is what it is about.
			ID:      m.ModelId,
			Concept: FleetModelConcept,
			Type:    memorynodes.NodeTypeObject,
			Payload: raw,
		})
	}
	return nodes, nil
}

// evaluateInferenceStatusExpression produces ONE row: can this caller actually
// get inference, and through which door.
//
// It is one row rather than a list because it answers one question, and the
// first-run gate needs an answer rather than material to compute one. Every
// input is named on the row so a person looking at a gate they do not
// understand can see what it read.
func (e *MemQLEngine) evaluateInferenceStatusExpression(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	if e == nil || e.providers == nil {
		return nil, nil
	}

	localModels := 0
	localEligible := false
	var eligibleModelIds []string
	models, err := e.fleetCatalogForCaller(ctx)
	if err == nil {
		for _, m := range models {
			localModels++
			// The MINIMUM CAPABILITY PROFILE (design G): structured output
			// plus the context floor. Not "any model at all" -- a fleet whose
			// only model cannot do structured output would pass a naive gate
			// and then refuse every conductor turn, which is a worse place to
			// put a person than the door they were on.
			if m.Online() && m.StructuredOutput && m.ContextWindow >= MinimumContextWindow {
				localEligible = true
				eligibleModelIds = append(eligibleModelIds, m.ModelId)
			}
		}
	}
	sort.Strings(eligibleModelIds)

	cloudConfigured := e.providers.HasCloudProviderConfigured()
	federation := e.providers.federationConfigured()

	doors := make([]any, 0, 3)
	if localEligible {
		doors = append(doors, InferenceDoorLocal)
	}
	if federation {
		doors = append(doors, InferenceDoorFederation)
	}
	if cloudConfigured && !federation {
		doors = append(doors, InferenceDoorApiKey)
	}

	raw, err := json.Marshal(map[string]any{
		"eligible":             localEligible || cloudConfigured || federation,
		"doorsOpen":            doors,
		"localEligible":        localEligible,
		"localModelCount":      localModels,
		"eligibleModelIds":     toAnySlice(eligibleModelIds),
		"cloudConfigured":      cloudConfigured,
		"federationConfigured": federation,
		// fleetInferenceInstalled distinguishes "your machines are asleep"
		// from "the node answering this request has no worker service at
		// all". They look identical from a page and have entirely different
		// fixes.
		"fleetInferenceInstalled": e.providers.FleetInferenceInstalled(),
		"minimumContextWindow":    MinimumContextWindow,
	})
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		// A single row answering a single question, so its id is a constant.
		ID:      "current",
		Concept: InferenceStatusConcept,
		Type:    memorynodes.NodeTypeObject,
		Payload: raw,
	}}, nil
}

// fleetCatalogForCaller reads the caller's own machines, merged with the
// shared-inference set.
//
// MERGED, NOT UNIONED BLINDLY: a model both sets offer appears ONCE, with the
// machine lists concatenated. Two entries for one model would render as two
// rows on the Providers page for something the router treats as one thing.
func (e *MemQLEngine) fleetCatalogForCaller(ctx context.Context) ([]FleetModel, error) {
	actingUserId := actingUserFromContext(ctx)

	byModel := map[string]*FleetModel{}
	seenMachine := map[string]bool{}

	add := func(models []FleetModel) {
		for _, m := range models {
			entry, ok := byModel[m.ModelId]
			if !ok {
				copied := m
				copied.Machines = nil
				entry = &copied
				byModel[m.ModelId] = entry
			}
			if m.ContextWindow > entry.ContextWindow {
				entry.ContextWindow = m.ContextWindow
			}
			entry.StructuredOutput = entry.StructuredOutput || m.StructuredOutput
			entry.Embeddings = entry.Embeddings || m.Embeddings
			for _, machine := range m.Machines {
				key := m.ModelId + "\x00" + machine.RegistrationId
				if seenMachine[key] {
					continue
				}
				seenMachine[key] = true
				entry.Machines = append(entry.Machines, machine)
			}
		}
	}

	if strings.TrimSpace(actingUserId) != "" {
		mine, err := e.providers.FleetCatalog(ctx, actingUserId)
		if err != nil {
			return nil, fmt.Errorf("fleet catalog: %w", err)
		}
		add(mine)
	}
	// The shared set is readable by everyone because everyone's SYSTEM work
	// may land on it. It is not the same as reading another user's fleet: its
	// owners opted these machines in to cluster use.
	shared, err := e.providers.FleetCatalog(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("shared fleet catalog: %w", err)
	}
	add(shared)

	out := make([]FleetModel, 0, len(byModel))
	for _, m := range byModel {
		sort.Slice(m.Machines, func(i, j int) bool {
			return m.Machines[i].RegistrationId < m.Machines[j].RegistrationId
		})
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelId < out[j].ModelId })
	return out, nil
}

// federationConfigured reports whether any registered provider resolved its
// credential through workload identity federation (memql#4333) -- the second
// of the three inference doors.
func (r *ProviderRegistry) federationConfigured() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, entry := range r.byName {
		if entry == nil || !entry.Available {
			continue
		}
		// The SAME predicate providerAuthSourceOf reads, through the same
		// helper rather than re-derived: "what counts as federated" must have
		// one definition, or the Providers page and the first-run gate can
		// disagree about whether a door is open.
		if providerAuthSourceOf(entry) == AuthSourceFederation {
			return true
		}
	}
	return false
}
