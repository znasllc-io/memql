package fleetcatalog

import (
	"context"
	"sort"
	"strings"
	"time"

	memqlengine "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// Reader uses the same owner/shared store interface as dispatch, without a stream.
type Reader struct {
	Store StoreReader
	Now   func() time.Time
}
type StoreReader interface {
	WorkersForOwner(context.Context, string) ([]Candidate, error)
}
type SharedStoreReader interface {
	SharedInferenceWorkers(context.Context) ([]Candidate, error)
}

func (r *Reader) Catalog(ctx context.Context, owner string) ([]memqlengine.FleetModel, error) {
	if r == nil || r.Store == nil {
		return nil, nil
	}
	var machines []Candidate
	var err error
	if strings.TrimSpace(owner) == "" {
		shared, ok := r.Store.(SharedStoreReader)
		if !ok {
			return nil, nil
		}
		machines, err = shared.SharedInferenceWorkers(ctx)
		if err != nil {
			return nil, err
		}
		kept := machines[:0]
		for _, m := range machines {
			if m.ServesCluster() {
				kept = append(kept, m)
			}
		}
		machines = kept
	} else {
		machines, err = r.Store.WorkersForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	return Project(machines, now), nil
}

// projectCatalog turns registrations into one entry per model.
//
// A model appears when ANY machine advertises it, and its attributes are the
// UNION over the machines behind it: if one machine can do structured output
// for llama3.1:8b, the model can, because a call needing it will be routed to
// that machine. Taking the intersection instead would hide a capability the
// fleet actually has whenever one machine under-reports.
//
// OFFLINE MACHINES STAY IN THE PROJECTION, marked offline. A model whose only
// machine is asleep must still be visible: the operator's question is "why is
// my model not being used", and an entry that vanished answers it with
// silence. Selection reads Online(), so an all-offline model is unavailable
// without being invisible.
func Project(machines []Candidate, now time.Time) []memqlengine.FleetModel {
	byModel := map[string]*memqlengine.FleetModel{}
	for _, m := range machines {
		// Ask / Setup / catalog "online" means a replica holds the stream now
		// (connectedNodeId), not merely that a heartbeat was seen recently and
		// never that activeCount > 0.
		online := workerservice.StreamHeld(m.ConnectedNodeId, m.RevokedAt)
		runtimes := m.Runtimes()
		for _, modelId := range m.ModelsOffered() {
			attrs, _ := m.ModelAttributesFor(modelId)
			// Validate against this machine's total before another machine's
			// larger total can make an impossible active count appear valid.
			if attrs.Params > 0 && attrs.ActiveParams > attrs.Params {
				attrs.ActiveParams = 0
			}
			entry, ok := byModel[modelId]
			if !ok {
				entry = &memqlengine.FleetModel{ModelId: modelId}
				byModel[modelId] = entry
			}
			// The union, per the note above.
			if attrs.ContextWindow > entry.ContextWindow {
				entry.ContextWindow = attrs.ContextWindow
			}
			entry.StructuredOutput = entry.StructuredOutput || attrs.StructuredOutput
			entry.Embeddings = entry.Embeddings || attrs.Embeddings
			entry.Tools = entry.Tools || attrs.Tools
			// The four modality flags fold the same way (epic memql#5137, D4):
			// one machine that can see makes the MODEL servable for vision, and
			// selection then picks that machine specifically.
			entry.Vision = entry.Vision || attrs.Vision
			entry.AudioIn = entry.AudioIn || attrs.AudioIn
			entry.AudioOut = entry.AudioOut || attrs.AudioOut
			entry.ImageGen = entry.ImageGen || attrs.ImageGen
			// MAX of params across the machines behind the model, for the
			// same reason every capability is a union: a machine that
			// under-reports must not shrink a model the fleet demonstrably
			// runs at full size.
			if attrs.Params > entry.Params {
				entry.Params = attrs.Params
			}
			if attrs.ActiveParams > entry.ActiveParams {
				entry.ActiveParams = attrs.ActiveParams
			}
			// The quantization is the FIRST non-empty one reported, and it
			// is operator-facing only. Two machines running different
			// quantizations of one model are the same model to a caller, so
			// there is nothing to reconcile -- and a concatenated list would
			// read as a claim about the model rather than about a machine.
			if entry.Quant == "" {
				entry.Quant = attrs.Quant
			}
			entry.Machines = append(entry.Machines, memqlengine.FleetMachine{
				RegistrationId: m.RegistrationId,
				Name:           m.Name,
				DisplayName:    m.DisplayName,
				OwnerUserId:    m.OwnerUserId,
				Runtimes:       runtimes,
				Online:         online,
				ActiveCount:    m.ActiveCount,
				MaxConcurrent:  attrs.MaxConcurrent,
				// What the machine IS, as against what it is currently serving.
				// Both were in scope here and dropped on the floor, which is why
				// the catalog's machine-class floor was never checked (memql#5195):
				// `minMachineClass` was compared against a fleet size nothing
				// reported. Hardware is the zero value for a cockpit that
				// predates the scanner, and UsableGigabytes answers 0 for it --
				// which every reader downstream takes as "has not said" rather
				// than as "has no memory".
				MemoryGb: memqlengine.UsableGigabytes(memqlengine.HardwareFromRow(m.Hardware.Row())),
				Platform: memqlengine.NormalizePlatform(m.Labels["os"]),
			})
		}
	}
	out := make([]memqlengine.FleetModel, 0, len(byModel))
	for _, e := range byModel {
		sort.Slice(e.Machines, func(i, j int) bool {
			return e.Machines[i].RegistrationId < e.Machines[j].RegistrationId
		})
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelId < out[j].ModelId })
	return out
}
