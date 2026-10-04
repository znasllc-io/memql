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
	"github.com/znasllc-io/memql/core/common"
)

// FleetModelConcept is the canonical id of the catalog projection.
const FleetModelConcept = "v1:platform:fleetModel"

// InferenceStatusConcept is the canonical id of the eligibility projection.
const InferenceStatusConcept = "v1:platform:inferenceStatus"

// The doors, as the status row names them.
//
// THE SET GAINED ONE AND LOST ONE IN THE SAME RELEASE, and the order below is
// the order the default chain tries them: a local model first, then an app the
// person already pays for, then federation. The list is what a client renders,
// so the order is part of the answer.
//
// `app` arrived with the app provider type (epic memql#5096, design D3).
// `apiKey` went with the keys (epic memql#5088): a cloud vendor is reached by
// workload identity federation or not at all, so a provider that is Available
// is a provider that federated, and its arm was already unreachable once the
// credential switch landed. An unreachable arm publishing a door NAME is worse
// than dead code, because the name is on the wire and a client may branch on
// it.
//
// FOUR DOORS, THREE NAMES: both vendors federate, and the row does not say
// which one answered. docs/public/operate/local-models.md numbers them for a
// reader; this enum is what a client switches on.
const (
	InferenceDoorLocal      = "local"
	InferenceDoorApp        = "app"
	InferenceDoorFederation = "federation"
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
				// memory and platform are what let the catalog CHECK a model's
				// machine-class floor. Before memql#5195 no field on this entry
				// carried either, so the floor was guarded on a fleet size that
				// was never reported and was therefore never checked, on any
				// fleet. Zero and "" mean the machine has not said.
				"memoryGb": int(mm.MemoryGb),
				"platform": mm.Platform,
			})
		}
		raw, err := json.Marshal(map[string]any{
			"modelId":          m.ModelId,
			"contextWindow":    m.ContextWindow,
			"structuredOutput": m.StructuredOutput,
			"embeddings":       m.Embeddings,
			"tools":            m.Tools,
			"vision":           m.Vision,
			"audioIn":          m.AudioIn,
			"audioOut":         m.AudioOut,
			"imageGen":         m.ImageGen,
			"params":           m.Params,
			"activeParams":     m.ActiveParams,
			"quant":            m.Quant,
			// The measured figures, in the discriminated shape the measurement
			// row stores (epic memql#5146). ABSENT AS A REASON, never as a
			// zero: a surface reading `measured: false` renders the sentence,
			// and there is no median beside the flag to be read by mistake.
			"measured":     measuredRow(m.Measured),
			"online":       m.Online(),
			"machineCount": len(m.Machines),
			"onlineCount":  online,
			"machines":     machines,
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

// inferenceDoors is the shared reading behind the inferenceStatus row and
// the `ai` readiness module. ONE implementation, two readers: a second would
// let a person be told inference is configured on one surface and not on the
// other, with both readings defensible.
type inferenceDoors struct {
	StreamingChatEligible bool
	LocalEligible         bool
	LocalModels           int
	EligibleModelIds      []string
	// AppEligible and RunnableApps are the app door (epic memql#5096): a
	// signed-in Claude Code or Codex on a machine this node can REACH -- one
	// whose stream this replica holds, or one held by an agent it can forward
	// a call to (AppDoor.Reachable, the chat door's predicate; inference is a
	// chat turn, and a chat turn crosses). They live here rather than beside
	// the caller for the reason the type's own note gives -- the readiness
	// verdict and the inferenceStatus row must not be able to disagree about
	// whether inference is configured.
	AppEligible          bool
	RunnableApps         []string
	AppSessionsInstalled bool
	CloudConfigured      bool
	Federation           bool
	Doors                []string
}

func (e *MemQLEngine) inferenceDoors(ctx context.Context) inferenceDoors {
	var d inferenceDoors
	if e == nil || e.providers == nil {
		return d
	}
	models, err := e.fleetCatalogForCaller(ctx)
	if err == nil {
		for _, m := range models {
			d.LocalModels++
			// Ask needs plain streaming chat, not structured output. Read the
			// same caller-scoped catalog on BFF even though its worker dispatch
			// lives on agent replicas. An embedding model cannot serve chat.
			if ok, _ := m.eligibleFor(FleetNeeds{MinContextWindow: MinimumContextWindow}); ok && !m.Embeddings {
				d.StreamingChatEligible = true
			}
			// The MINIMUM CAPABILITY PROFILE (design G): structured output
			// plus the context floor. Not "any model at all" -- a fleet whose
			// only model cannot do structured output would pass a naive gate
			// and then refuse every conductor turn, which is a worse place to
			// put a person than the door they were on.
			if m.Online() && m.StructuredOutput && m.ContextWindow >= MinimumContextWindow {
				d.LocalEligible = true
				d.EligibleModelIds = append(d.EligibleModelIds, m.ModelId)
			}
		}
	}
	sort.Strings(d.EligibleModelIds)

	// The app door. A read that FAILS leaves it SHUT rather than unknown:
	// this reading is what a gate branches on, and "we could not ask"
	// reported as an open door sends somebody to a console whose features
	// then refuse.
	d.AppSessionsInstalled = e.providers.AppInferenceInstalled()
	if doors, err := e.providers.AppDoors(ctx, actingUserFromContext(ctx)); err == nil {
		for _, door := range doors {
			// REACHABLE, not Runnable: Runnable is the session door's
			// local-only question (can a step handover open HERE), and this
			// row answers the chat door's -- can this caller get inference.
			if door.Reachable() {
				d.AppEligible = true
				d.RunnableApps = append(d.RunnableApps, door.AppId)
				// Runnable apps currently answer completed chat only. Do not
				// promise streaming merely because their sessions are ready.
				if _, streams := any((*appProvider)(nil)).(common.ChatStreamProvider); streams {
					d.StreamingChatEligible = true
				}
			}
		}
	}
	sort.Strings(d.RunnableApps)

	d.CloudConfigured = e.providers.HasCloudProviderConfigured()
	var federationStreams bool
	d.Federation, federationStreams = e.providers.federationReadiness()
	d.StreamingChatEligible = d.StreamingChatEligible || federationStreams
	// THE ORDER IS THE ORDER THE DEFAULT CHAIN TRIES THEM (design D4): a
	// model on the person's own hardware, then a subscription they already
	// pay for, then federation, then a key. Every client renders this list
	// rather than re-deriving one, so the order is part of the answer.
	if d.LocalEligible {
		d.Doors = append(d.Doors, InferenceDoorLocal)
	}
	if d.AppEligible {
		d.Doors = append(d.Doors, InferenceDoorApp)
	}
	if d.Federation {
		d.Doors = append(d.Doors, InferenceDoorFederation)
	}
	// CloudConfigured is still READ and still reported on the row -- it is
	// what the park card's cloud-approval affordance keys on -- but it is no
	// longer a door of its own. After epic memql#5088 a cloud provider is
	// Available only if it federated, so `CloudConfigured && !Federation` is
	// unreachable rather than merely unlikely, and appending a third door for
	// it would publish a name nothing can produce.
	return d
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

	d := e.inferenceDoors(ctx)

	raw, err := json.Marshal(map[string]any{
		"streamingChatEligible": d.StreamingChatEligible,
		"eligible":              d.LocalEligible || d.AppEligible || d.CloudConfigured || d.Federation,
		"doorsOpen":             toAnySlice(d.Doors),
		"localEligible":         d.LocalEligible,
		"appEligible":           d.AppEligible,
		"runnableApps":          toAnySlice(d.RunnableApps),
		"localModelCount":       d.LocalModels,
		"eligibleModelIds":      toAnySlice(d.EligibleModelIds),
		"cloudConfigured":       d.CloudConfigured,
		"federationConfigured":  d.Federation,
		// fleetInferenceInstalled distinguishes "your machines are asleep"
		// from "the node answering this request has no worker service at
		// all". They look identical from a page and have entirely different
		// fixes.
		"fleetInferenceInstalled": e.providers.FleetInferenceInstalled(),
		"fleetCatalogInstalled":   e.providers.FleetCatalogInstalled(),
		// The app door's twin of the line above, and the same distinction:
		// "you have signed into nothing" and "this node cannot open an app
		// session at all" look identical on a page and have different fixes.
		"appSessionsInstalled": d.AppSessionsInstalled,
		"minimumContextWindow": MinimumContextWindow,
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
			// A larger total from the other source must not rehabilitate
			// an active count that was invalid in its original report.
			if m.Params > 0 && m.ActiveParams > m.Params {
				m.ActiveParams = 0
			}
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
			entry.Tools = entry.Tools || m.Tools
			// The four modality flags, same union (epic memql#5137, D4).
			entry.Vision = entry.Vision || m.Vision
			entry.AudioIn = entry.AudioIn || m.AudioIn
			entry.AudioOut = entry.AudioOut || m.AudioOut
			entry.ImageGen = entry.ImageGen || m.ImageGen
			if m.Params > entry.Params {
				entry.Params = m.Params
			}
			if m.ActiveParams > entry.ActiveParams {
				entry.ActiveParams = m.ActiveParams
			}
			if entry.Quant == "" {
				entry.Quant = m.Quant
			}
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

	// The measured figures ride the SAME read as the catalog (epic memql#5146,
	// D4), rather than being fetched by whoever orders. Two readers of one fact
	// would let the page and the router disagree about which model is
	// strongest -- and the disagreement would be invisible, because both
	// answers are plausible.
	//
	// A FAILED MEASUREMENT READ IS NOT A FAILED CATALOG READ. Measurements rank
	// and gate nothing, so a catalog with no figures is the ordinary state of a
	// fleet nobody has probed; refusing the whole read because the ranking
	// input is missing would take local inference down over a page decoration.
	measurements, err := e.measurementsForCaller(ctx)
	if err != nil {
		return out, nil
	}
	return AttachMeasurements(out, measurements), nil
}

// measurementsForCaller reads every measurement the caller may see.
//
// It goes through the AUTHORIZED query rather than selecting rows here, so the
// concept's own tier decides what comes back: a plain user sees their own
// machines' figures, a cluster owner sees the fleet's, and this function does
// not have to be trusted to narrow anything.
func (e *MemQLEngine) measurementsForCaller(ctx context.Context) ([]Measurement, error) {
	res, err := e.Execute(ctx, "measurementsForCaller()")
	if err != nil {
		return nil, err
	}
	rows := modelPullRows(res.OutputPayload())
	out := make([]Measurement, 0, len(rows))
	for _, row := range rows {
		m := MeasurementFromRow(row)
		if m.ModelId == "" || m.MachineId == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// federationConfigured reports whether any registered provider resolved its
// credential through workload identity federation (memql#4333) -- the second
// of the three inference doors.
func (r *ProviderRegistry) federationConfigured() bool {
	configured, _ := r.federationReadiness()
	return configured
}

// federationReadiness distinguishes an available federated provider from one
// that can serve Ask's plain stream. Credentials alone cannot establish modality.
func (r *ProviderRegistry) federationReadiness() (configured, streamingChat bool) {
	if r == nil {
		return false, false
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
			configured = true
			if _, ok := entry.Client.(common.ChatStreamProvider); ok {
				streamingChat = true
			}
		}
	}
	return configured, streamingChat
}
