package memql

// The `fleet` provider type: local models, on the user's own machines
// (epic memql#4676, task memql#4679).
//
// WHY THERE ARE NO STATIC PER-MODEL PROVIDERS. Every other provider in the
// tree is a record an author wrote: a vendor, a model id, a price. A fleet
// model is none of those things -- it exists while a laptop is awake and
// stops existing when the lid closes, and which laptop is a different answer
// for every user. So there is ONE base provider (`@base @type("Fleet")
// provider fleet {}`) and the models are resolved from a live catalog at
// SELECTION time.
//
// THE SEAM, AND WHY IT IS A SEAM. Model calls travel over the WorkerService
// stream, which terminates on the agent node -- so the code that can actually
// place one is behind `//go:build agent`. component/memql is untagged and is
// linked into every binary. If the provider reached for the router directly,
// this package would not compile off the agent build; if it were declared only
// there, every other node would silently have no fleet at all and would report
// that as "no local model available", which is the refusal this epic invented
// to mean something else. So the contract lives here and the implementation is
// installed by whichever build has a worker service.
//
// A NODE WITH NO FLEET INFERENCE INSTALLED HAS AN UNAVAILABLE FLEET, NOT A
// BROKEN ONE. That is the same state as "no machine is awake", and it flows
// through the same path: the entry resolves, reports Available=false, and the
// policy's authored fallback runs -- or, with none authored, the call is
// refused and the work parks (memql#4682). Nothing throws.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// FleetProviderType is the @type annotation value on the base provider.
const FleetProviderType = "Fleet"

// FleetProviderName is the base provider's registry name.
const FleetProviderName = "fleet"

// FleetReferencePrefix is how a policy names a fleet model:
// `@primary("fleet:llama3.1:8b")`.
const FleetReferencePrefix = "fleet:"

// FleetWildcard is the RETIRED reference `fleet:*`. It is kept as a constant
// for exactly one purpose: recognising it, so it can be refused by name with
// the replacement spelled out (design D4).
//
// It used to mean "any eligible local model, strongest first". That reading
// survives under the name `fleet:strongest`, and the retirement is what makes
// room for a second question about the same fleet -- `fleet:fastest` -- which
// `*` could never have expressed: a wildcard says which models are allowed,
// and the selectors say which of them to prefer.
const FleetWildcard = FleetReferencePrefix + "*"

// Fleet SELECTORS. A selector is the part after `fleet:` when it names an
// ORDERING over the live catalog rather than one model id.
//
// They exist because the alternative is a policy that names one model id, and
// a model id is the one thing an operator cannot know in advance: which
// weights are pulled is a decision made on each machine, and a chain pinned
// to `fleet:llama3.1:8b` parks on a fleet running qwen2.5:7b that would have
// served the turn perfectly.
const (
	// FleetSelectorStrongest orders the catalog strongest-first: the owner's
	// explicit preference, then parameters, then context window, missing
	// attributes last.
	FleetSelectorStrongest = "strongest"
	// FleetSelectorFastest orders it fastest-first: measured throughput when
	// there is any, else FEWEST parameters, missing attributes last.
	FleetSelectorFastest = "fastest"
)

// FleetStrongest and FleetFastest are the whole references a policy writes.
const (
	FleetStrongest = FleetReferencePrefix + FleetSelectorStrongest
	FleetFastest   = FleetReferencePrefix + FleetSelectorFastest
)

// IsFleetSelector reports whether a reference names an ordering over the live
// catalog rather than one model, and returns which ordering.
//
// A model id is never mistaken for a selector, because the two selector words
// are reserved: a runtime that published a model literally called `strongest`
// would be unreachable by id, which is a trade nobody will ever make and is
// cheaper than a grammar where the meaning of `fleet:x` depends on what is
// installed today.
func IsFleetSelector(name string) (string, bool) {
	modelId, ok := IsFleetReference(name)
	if !ok {
		return "", false
	}
	switch modelId {
	case FleetSelectorStrongest, FleetSelectorFastest:
		return modelId, true
	}
	return "", false
}

// Model call kinds, mirroring the wire.
//
// SIX (epic memql#5137, D4). The four beyond chat and embedding are what give
// vision, transcription, speech and image generation a LOCAL door: before them
// the fleet wire carried two kinds, so every other modality reached a paid
// vendor or did not happen at all.
//
// These strings must equal component/worker's ModelCallKind* constants and the
// cockpit's. They are duplicated here rather than imported because
// component/worker is the wire package and this is the provider -- and the
// duplication is guarded by TestFleetKindsMatchTheWireKinds rather than by
// hoping.
const (
	FleetKindChat       = "chat"
	FleetKindEmbedding  = "embedding"
	FleetKindVision     = "vision"
	FleetKindTranscribe = "transcribe"
	FleetKindSpeak      = "speak"
	FleetKindImage      = "image"
)

// ErrFleetUnavailable is what a fleet call returns when no eligible machine
// could serve it. It is deliberately ONE error for two situations -- no
// machine offers the model, and none that offers it has the capability this
// prompt needs -- because they are the same answer to a caller: the provider
// is unavailable. memql#4682 turns it into the typed refusal an operator
// reads, with the machines considered named.
var ErrFleetUnavailable = errors.New("no eligible fleet machine for this model")

// FleetMachine is one machine behind a catalog entry.
type FleetMachine struct {
	RegistrationId string
	Name           string
	DisplayName    string
	// OwnerUserId is populated only on the cluster-wide (shared-inference)
	// read; on a user's own catalog every machine is theirs.
	OwnerUserId   string
	Runtimes      []string
	Online        bool
	ActiveCount   int
	MaxConcurrent uint32
	// MemoryGb is what the machine has to give a model, in whole gigabytes, as
	// UsableGigabytes computes it -- VRAM on a discrete card, 75 percent of the
	// pool on unified memory. ZERO MEANS THE COCKPIT HAS NOT REPORTED, never a
	// machine with no memory: a cockpit that predates the hardware scanner sends
	// no inventory at all, and every reader must take the zero that way or it
	// will tell somebody their machine is too small when nobody has asked it yet.
	MemoryGb uint64
	// Platform is the machine's operating system in the CATALOG's vocabulary
	// (`macos`, `linux`), normalized here so the wire carries one spelling --
	// the machine itself reports Go's GOOS, which says `darwin`. Empty when the
	// machine has not said, which blocks nothing.
	Platform string
}

// Busy reports whether the machine is at its declared ceiling for this model.
// A machine that declared no ceiling is never busy -- it asked for no limit,
// so the limit is not the thing to describe it by.
func (m FleetMachine) Busy() bool {
	return m.MaxConcurrent > 0 && uint32(m.ActiveCount) >= m.MaxConcurrent
}

// FleetModel is one entry in the live catalog: a model, its attributes, and
// the machines behind it.
type FleetModel struct {
	ModelId       string
	ContextWindow int
	// Params is the model's parameter count, as the runtime reported it
	// (Ollama's `details.parameter_size`, e.g. "8B" -> 8_000_000_000).
	//
	// Used for ordering, and as the fallback when ActiveParams is unknown.
	// The automatic fastest selector imposes a size floor; concrete model
	// pins retain the capabilities the machine advertised.
	Params int64
	// ActiveParams is the per-token parameter count for a mixture; zero means unreported.
	ActiveParams int64
	// Quant is the quantization level the runtime reported (Q4_K_M, F16).
	// Recognized precision breaks strongest ties after effective parameters
	// and context, so a retained Q4 does not displace an upgraded Q8 variant.
	Quant            string
	StructuredOutput bool
	Embeddings       bool
	// Tools reports that at least one machine behind this model can carry a
	// tool-calling turn. FALSE IS THE DEFAULT and it is a gate, not a
	// downgrade: a runtime handed tools it cannot honour answers prose,
	// which surfaces three layers away as an agent that stopped using its
	// tools for no reason a reader can see.
	Tools bool
	// The four MODALITY flags (epic memql#5137, D4), each a union across the
	// machines behind this model, exactly as Tools and StructuredOutput are.
	//
	// FALSE IS THE DEFAULT AND IT IS A GATE, for the reason on Tools above: a
	// machine that never advertised vision is SKIPPED for a vision turn rather
	// than picked and left to fail. The union is right because the question is
	// "can this fleet serve a vision turn on this model", and one machine that
	// can is enough -- selection then picks that machine specifically.
	Vision   bool
	AudioIn  bool
	AudioOut bool
	ImageGen bool
	// Measured is what a probe found this model actually DOES on the machines
	// behind it (epic memql#5146, D4). Its zero value is "nobody has probed
	// this", which is a different answer from a measured zero and sorts
	// differently -- see fleet_measured.go, where both accessors return
	// `(value, ok)` and every caller reads the ok first.
	//
	// It RANKS and it gates nothing this release: eligibility stays with the
	// advertised flags above, so a model that failed the structured probe is
	// still eligible for a structured call and is reported as failing.
	Measured Measured
	Machines []FleetMachine
}

// Online reports whether at least one machine behind this model is reachable.
func (m FleetModel) Online() bool {
	for _, mm := range m.Machines {
		if mm.Online {
			return true
		}
	}
	return false
}

// FleetNeeds is what a particular call requires of a model. It mirrors the
// router's ModelNeeds without importing it: this package cannot see the
// agent-tagged one.
type FleetNeeds struct {
	StructuredOutput bool
	Embeddings       bool
	Tools            bool
	MinContextWindow int
	// The four MODALITY needs, derived from the call kind by Needs() above
	// (epic memql#5137, D4). Same direction as the three above: an
	// unadvertised capability rules a machine out rather than degrading the
	// call.
	Vision   bool
	AudioIn  bool
	AudioOut bool
	ImageGen bool
}

// FleetCallRequest is one model call.
type FleetCallRequest struct {
	// ActingUserId scopes the call to that user's machines. EMPTY MEANS
	// SYSTEM WORK, which reaches only machines whose owner opted in -- it
	// does not mean "any machine". The two paths are separate all the way
	// down (memql#4678).
	ActingUserId string
	// RegistrationId strictly limits dispatch to one of ActingUserId's own
	// machines. Empty retains normal fleet routing, including shared machines.
	RegistrationId string
	ModelId        string
	Kind           string
	Messages       []common.ChatMessage
	// ContextTokens is the requested runtime window, also a machine eligibility floor.
	ContextTokens int
	// Schema is set for a structured call; its presence is also what makes
	// the call require a structured-output-capable model.
	Schema *common.StructuredSchema
	// Tools are the functions this turn offers the model. Their presence is
	// also what makes the call require a tool-capable model, the same way
	// Schema's presence requires a structured-output one.
	Tools          []common.ToolDefinition
	EmbeddingInput []string
	// The four MODALITY payloads (epic memql#5137, D4). Each is set for exactly
	// one kind and nil for the rest -- the KIND is what says which, so a
	// populated field on the wrong kind is ignored rather than reinterpreted.
	//
	// Images ride the request rather than the message here, unlike on the wire,
	// because a FleetCallRequest carries one turn: the wire needs the
	// per-message form so a multi-turn conversation can say which turn an image
	// belonged to, and this side has only ever built single-turn vision calls.
	Images  []FleetImage
	Audio   *FleetAudio
	Speech  *FleetSpeechRequest
	Image   *FleetImageRequest
	Purpose string
	RunId   string
	StepId  string
	// OnDelta, when set, receives streamed content as it arrives.
	OnDelta func(string)
}

// Needs derives the capability requirement from the request itself, so a
// caller cannot ask for structured output and forget to require a model that
// can do it.
func (r FleetCallRequest) Needs() FleetNeeds {
	return FleetNeeds{
		StructuredOutput: r.Schema != nil,
		MinContextWindow: r.ContextTokens,
		Embeddings:       r.Kind == FleetKindEmbedding,
		Tools:            len(r.Tools) > 0,
		// The four modality needs come from the KIND, which is the only thing
		// that can say them: a vision call is a vision call because of what it
		// carries, not because the caller remembered to ask for a seeing model.
		// Deriving them here rather than at each call site is what stops one
		// site from forgetting -- the failure being a turn routed to a machine
		// that cannot serve it.
		Vision:   r.Kind == FleetKindVision,
		AudioIn:  r.Kind == FleetKindTranscribe,
		AudioOut: r.Kind == FleetKindSpeak,
		ImageGen: r.Kind == FleetKindImage,
	}
}

// FleetUsage is what the runtime reported. Known=false means it reported
// nothing, which the ledger records as `unknown` rather than as zero.
type FleetUsage struct {
	InputTokens  int64
	OutputTokens int64
	Known        bool
	Model        string
}

// FleetCallResult is the answer.
type FleetCallResult struct {
	Content    string
	Embeddings [][]float32
	// ToolCalls are the calls the model made this turn, empty when it
	// answered in prose.
	ToolCalls []common.ToolCall
	Usage     FleetUsage
	// ExecutionSurface names the machine that served the call, in the
	// `fleet:<registrationId>` form the ledger stores (memql#4681).
	ExecutionSurface string
	// MachineLabel is the human-readable name for a card or a log line.
	MachineLabel string
	// MachineOwnerUserId names WHOSE MACHINE served the call, and ONLY when
	// that owner is not the caller (epic memql#5327, design D15).
	//
	// EMPTY ON YOUR OWN MACHINE, deliberately, and repeating userId there
	// would be the easy alternative and the wrong one: the field answers
	// "whose machine, if not yours", so filling in the self case would turn
	// "is this a shared call" from a read into a comparison -- and every
	// reader would have to make it.
	MachineOwnerUserId string
	// The three MODALITY results (epic memql#5137, D4). Segments accompany a
	// transcription's Content; Audio is a speak result; Images an image one.
	//
	// A transcription's TEXT is on Content rather than in a fourth field,
	// because it is the same thing every other kind puts there and a caller
	// that only wants the words should not have to know which kind produced
	// them.
	Segments []FleetTranscriptSegment
	Audio    *FleetAudio
	Images   []FleetImage
}

// FleetCatalogReader reads graph-backed availability without dispatching calls.
// Both public query nodes and agent nodes install the same projection.
type FleetCatalogReader interface {
	Catalog(context.Context, string) ([]FleetModel, error)
}

// SetFleetCatalog installs a read-only catalog without claiming local dispatch.
func (r *ProviderRegistry) SetFleetCatalog(reader FleetCatalogReader) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fleetCatalog = reader
}

// FleetCatalogInstalled reports whether this node can read fleet availability.
func (r *ProviderRegistry) FleetCatalogInstalled() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fleetCatalog != nil
}

// FleetInference is the contract an agent-tagged build fills in.
type FleetInference interface {
	// Catalog returns the live model list. An empty actingUserId asks for
	// the shared-inference set (machines whose owners opted in to cluster
	// work), never for "everything".
	Catalog(ctx context.Context, actingUserId string) ([]FleetModel, error)
	// Call runs one model call, streaming through req.OnDelta when set.
	// ErrFleetUnavailable when no eligible machine could serve it.
	Call(ctx context.Context, req FleetCallRequest) (FleetCallResult, error)
	// ModelPreference returns the owner's explicit ordered model list from
	// their routing policy, or nil when they have none -- which is most
	// users, and is why the default ordering has to be good on its own.
	//
	// It is a SEAM METHOD rather than a field on FleetModel because it is a
	// property of the CALLER, not of a model: two users looking at the same
	// machine can legitimately want different models tried first.
	ModelPreference(ctx context.Context, actingUserId string) ([]string, error)
}

// SetFleetInference installs the implementation. Called once during cluster
// wiring on a build that has a worker service; every other build leaves it
// nil and reports an unavailable fleet.
func (r *ProviderRegistry) SetFleetInference(f FleetInference) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fleet = f
	r.fleetCatalog = f
}

// FleetInferenceInstalled reports whether this node can place model calls on
// the fleet at all. The portal's eligibility check reads it so "this node has
// no worker service" is distinguishable from "your machines are asleep".
func (r *ProviderRegistry) FleetInferenceInstalled() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fleet != nil
}

// FleetCatalog returns the live catalog for an acting user, or the
// shared-inference set when actingUserId is empty.
func (r *ProviderRegistry) FleetCatalog(ctx context.Context, actingUserId string) ([]FleetModel, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	f := r.fleetCatalog
	r.mu.RUnlock()
	if f == nil {
		return nil, nil
	}
	models, err := f.Catalog(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ModelId < models[j].ModelId })
	return models, nil
}

// IsFleetReference reports whether a policy's provider name refers to the
// fleet at all, and returns whatever followed the prefix -- a model id, a
// selector word, or the retired "*". Callers that need to tell those apart ask
// IsFleetSelector and IsFleetWildcard.
func IsFleetReference(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, FleetReferencePrefix) {
		return "", false
	}
	modelId := strings.TrimSpace(strings.TrimPrefix(name, FleetReferencePrefix))
	return modelId, modelId != ""
}

// IsFleetWildcard reports whether a reference is the RETIRED `fleet:*`.
//
// It is a recognizer, not a resolution path. Nothing resolves a wildcard any
// more: fleetEntry refuses it by name so an author who wrote the old spelling
// is told what to write instead, rather than getting an entry that behaves
// almost like `fleet:strongest` and diverges the first time somebody asks for
// `fleet:fastest`.
func IsFleetWildcard(name string) bool {
	return strings.TrimSpace(name) == FleetWildcard
}

// orderModels ranks a catalog strongest-first (design D5).
//
// The order is: the caller's explicit preference for the ids it names, then
// ACTIVE PARAMETERS (total when unreported) descending, then CONTEXT WINDOW
// descending, then quantization precision descending, then model id.
//
// MISSING ATTRIBUTES SORT LAST, NEVER FIRST, and that direction is the whole
// of the rule. A model that does not say how big it is must not win by
// silence: a cockpit that predates the attribute, or a runtime that reports
// nothing, would otherwise become the fleet's strongest model on every
// machine it runs on. Sorting it last costs it a turn it might have served;
// sorting it first would quietly route every planning turn to a 1B model.
//
// The sort is STABLE over the input order, so a fleet whose models tie on
// every signal is ordered identically on every replica -- the property the
// routing strategies already depend on.
func orderModels(models []FleetModel, preference []string) []FleetModel {
	out := make([]FleetModel, len(models))
	copy(out, models)

	rank := make(map[string]int, len(preference))
	for i, id := range preference {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	// A model the preference does not name sorts after every one it does.
	prefRank := func(m FleetModel) int {
		if r, ok := rank[m.ModelId]; ok {
			return r
		}
		return len(preference) + 1
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// MEASURED CAPABILITY IS THE FIRST KEY (epic memql#5146, D4), above the
		// caller's own preference list. A preference names models a person
		// chose; this says which of them actually validates against this
		// cluster's schemas on the hardware that would serve the call, which is
		// the question the preference was a proxy for.
		//
		// THE BOOL IS READ BEFORE THE NUMBER, so a measured model outranks an
		// unmeasured one before any value is compared. A measured ZERO still
		// outranks unmeasured: it is a model somebody looked at, and rewarding
		// never being measured is the one direction this ordering must not take.
		if av, aok := ValidityOf(a); true {
			bv, bok := ValidityOf(b)
			if aok != bok {
				return aok
			}
			if aok && av != bv {
				return av > bv
			}
		}
		if ra, rb := prefRank(a), prefRank(b); ra != rb {
			return ra < rb
		}
		// Unknown size last, in both directions: it is not "zero
		// parameters", it is "the machine did not say".
		if (a.EffectiveParams() > 0) != (b.EffectiveParams() > 0) {
			return a.EffectiveParams() > 0
		}
		if a.EffectiveParams() != b.EffectiveParams() {
			return a.EffectiveParams() > b.EffectiveParams()
		}
		if a.ContextWindow != b.ContextWindow {
			return a.ContextWindow > b.ContextWindow
		}
		if aq, bq := quantPrecision(a.Quant), quantPrecision(b.Quant); aq != bq {
			return aq > bq
		}
		return a.ModelId < b.ModelId
	})
	return out
}

// orderModelsFastest ranks a catalog fastest-first.
//
// The order is: MEASURED THROUGHPUT when there is any, then FEWEST
// EFFECTIVE PARAMETERS (active when reported, otherwise total), then model id.
// There is no comparable measured throughput available here --
// modality-specific throughput figures use different units, so the first key
// remains a hook with a nil body and a name, deliberately left visible rather than
// written as a proxy: a number computed from parameters and called throughput
// would read on the decision record exactly like a measurement.
//
// MISSING PARAMETERS SORT LAST HERE TOO, and that is the same rule as
// orderModels rather than its mirror. "The machine did not say" is not "zero
// parameters": treating an unknown size as zero would make every model that
// failed to report itself the fastest thing on the fleet, which is the
// silence-wins failure orderModels exists to avoid, arriving from the other
// direction.
//
// The owner's model preference is deliberately NOT consulted. A preference
// list answers "which model do I want", which is the question `strongest`
// already asks; letting it win here would make `fleet:fastest` and
// `fleet:strongest` return the same model on every fleet whose owner set one
// -- exactly the fleets where the distinction was worth writing down.
//
// The sort is STABLE over the input order, so two replicas reading one
// catalog agree with no shared state.
func orderModelsFastest(models []FleetModel) []FleetModel {
	out := make([]FleetModel, len(models))
	copy(out, models)

	// throughputOf is the hook. It answers "not measured" for every model
	// today; epic 4 fills it from what the machines report.
	throughputOf := func(FleetModel) (float64, bool) { return 0, false }

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ta, okA := throughputOf(a)
		tb, okB := throughputOf(b)
		if okA != okB {
			return okA
		}
		if okA && okB && ta != tb {
			return ta > tb
		}
		// Unknown size last, in both directions.
		if (a.EffectiveParams() > 0) != (b.EffectiveParams() > 0) {
			return a.EffectiveParams() > 0
		}
		if a.EffectiveParams() != b.EffectiveParams() {
			return a.EffectiveParams() < b.EffectiveParams()
		}
		return a.ModelId < b.ModelId
	})
	return out
}

// orderBySelector applies the ordering a selector names. An unknown selector
// is an ERROR rather than a default, because a defaulted ordering is a
// routing decision nobody wrote: a typo in a policy would silently mean
// "strongest" and the chain would look correct.
func orderBySelector(models []FleetModel, selector string, preference []string) ([]FleetModel, error) {
	switch selector {
	case FleetSelectorStrongest:
		return orderModels(models, preference), nil
	case FleetSelectorFastest:
		return orderModelsFastest(models), nil
	}
	return nil, fmt.Errorf("unknown fleet selector %q: a fleet selector is one of %s, %s",
		selector, FleetSelectorStrongest, FleetSelectorFastest)
}

// FleetCandidate is one model from the live catalog, in the order a selector
// put it, carrying either its eligibility or the reason it cannot serve the
// call.
//
// Every model is returned, including the ones that were ruled out. The router
// records a reason per candidate on the decision, and a candidate list that
// silently omitted the misses would leave "your fleet cannot do this" with
// nothing behind it -- which is the sentence memql#4682 exists to replace.
type FleetCandidate struct {
	ModelId  string
	Eligible bool
	// Reason is why this model cannot serve the call. Empty when Eligible.
	Reason string
}

// FleetCandidatesFor resolves a fleet SELECTOR against the live catalog for
// one acting user, ordered by the selector and marked against these needs.
//
// It is the resolve-time half of fleet selection, and it exists because the
// decision record has to name a MODEL. The call-time chooser inside
// fleetProvider knows the needs of the call it is about to place but has
// nowhere to record what it passed over; this one knows the request's needs,
// including the context-window floor the call-time path cannot see, and hands
// the whole considered list back.
//
// An empty actingUserId asks for the shared-inference set -- machines whose
// owners opted in to cluster work -- never for "everything".
func (r *ProviderRegistry) FleetCandidatesFor(ctx context.Context, actingUserId, selector string, needs FleetNeeds) ([]FleetCandidate, error) {
	if r == nil {
		return nil, fmt.Errorf("no provider registry")
	}
	r.mu.RLock()
	f := r.fleet
	r.mu.RUnlock()
	if f == nil {
		return nil, fmt.Errorf("this node has no fleet inference installed")
	}
	models, err := f.Catalog(ctx, actingUserId)
	if err != nil {
		return nil, err
	}

	var preference []string
	if selector == FleetSelectorStrongest {
		// A policy read that failed must not decide the model. Falling back
		// to the default ordering is the honest degrade: the caller gets the
		// strongest eligible model rather than a refusal over a row nobody
		// asked about.
		preference, _ = f.ModelPreference(ctx, actingUserId)
	}

	ordered, err := orderBySelector(models, selector, preference)
	if err != nil {
		return nil, err
	}
	out := make([]FleetCandidate, 0, len(ordered))
	for _, m := range ordered {
		ok, why := m.eligibleForSelector(selector, needs)
		out = append(out, FleetCandidate{ModelId: m.ModelId, Eligible: ok, Reason: why})
	}
	return out, nil
}

// FleetFastMinParams is a conservative size floor for automatic fast selection.
// It excludes sub-billion toy models without depending on the model catalog or
// this call's serving machine class. This is a routing heuristic, not a quality
// benchmark. A concrete fleet:<id> reference remains the explicit escape hatch.
const FleetFastMinParams int64 = 3_000_000_000

// EffectiveParams preserves total weight size for capacity planning while using
// the per-token count for mixture ranking. Invalid active counts do not inflate
// a model beyond its total. Dense or unreported models use their total count.
func (m FleetModel) EffectiveParams() int64 {
	if m.ActiveParams > 0 && (m.Params <= 0 || m.ActiveParams <= m.Params) {
		return m.ActiveParams
	}
	return m.Params
}

func (m FleetModel) eligibleForSelector(selector string, needs FleetNeeds) (bool, string) {
	if selector == FleetSelectorFastest && !needs.AudioIn && !needs.AudioOut && m.EffectiveParams() < FleetFastMinParams {
		return false, fmt.Sprintf("effective parameters %d are under the fast quality floor %d; pin fleet:%s to override", m.EffectiveParams(), FleetFastMinParams, m.ModelId)
	}
	return m.eligibleFor(needs)
}

// eligibleFor reports whether a model can serve a call with these needs, and
// names the miss when it cannot. It mirrors ModelAttributes.Satisfies on the
// agent side -- the same questions asked of the CATALOG rather than of one
// machine's label, which is what the wildcard resolver needs.
func (m FleetModel) eligibleFor(n FleetNeeds) (bool, string) {
	if !m.Online() {
		return false, "no machine offering it is online"
	}
	if n.StructuredOutput && !m.StructuredOutput {
		return false, "does not advertise structured output"
	}
	if n.Embeddings && !m.Embeddings {
		return false, "does not advertise embeddings"
	}
	if n.Tools && !m.Tools {
		return false, "does not advertise tool calling"
	}
	if n.Vision && !m.Vision {
		return false, "does not advertise vision"
	}
	if n.AudioIn && !m.AudioIn {
		return false, "does not advertise audio input"
	}
	if n.AudioOut && !m.AudioOut {
		return false, "does not advertise audio output"
	}
	if n.ImageGen && !m.ImageGen {
		return false, "does not advertise image generation"
	}
	if n.MinContextWindow > 0 && m.ContextWindow < n.MinContextWindow {
		return false, fmt.Sprintf("context window %d is under the floor %d", m.ContextWindow, n.MinContextWindow)
	}
	return true, ""
}

// fleetEntry synthesizes the registry entry for `fleet:<modelId>`.
//
// It is resolved HERE, in Entry, rather than at load, and that placement is
// the whole of the "resolved at selection time, never at load" requirement: an
// offline fleet must not refuse boot, and a machine waking up must not need a
// reload to become usable. Every existing accessor -- providerByName,
// ChatStructuredProviderByName, the policy chain walk -- goes through Entry,
// so the fleet reaches all of them with no second lookup path.
//
// Availability is decided against the CATALOG, which means a call carrying no
// acting user resolves against the shared-inference set. That is correct
// rather than convenient: a system call may only use opted-in machines, so
// asking whether a user's laptop offers the model would answer a question
// nobody asked.
func (r *ProviderRegistry) fleetEntry(ctx context.Context, actingUserId, modelId string) (*ProviderConfigEntry, bool) {
	r.mu.RLock()
	f := r.fleet
	r.mu.RUnlock()

	cfg := ProviderConfig{
		Name:  FleetReferencePrefix + modelId,
		Type:  FleetProviderType,
		Model: modelId,
	}
	selector, isSelector := IsFleetSelector(cfg.Name)
	client := &fleetProvider{registry: r, modelId: modelId, actingUserId: actingUserId, selector: selector}
	entry := &ProviderConfigEntry{Config: cfg, Client: client}

	// `fleet:*` IS RETIRED, and it is refused HERE rather than resolved as a
	// synonym for `fleet:strongest` (design D4). A synonym would work right up
	// to the day somebody wrote `fleet:fastest` beside it and could not say
	// what the star meant any more; the refusal names the replacement, which
	// is the whole reason the constant survives.
	if modelId == "*" {
		entry.err = fmt.Errorf("%s is retired: write %s for the strongest eligible local model, or %s for the quickest",
			FleetWildcard, FleetStrongest, FleetFastest)
		return entry, true
	}

	if f == nil {
		// No worker service on this node. UNAVAILABLE, not an error: the
		// authored fallback runs, or the work parks.
		entry.err = fmt.Errorf("this node has no fleet inference installed")
		return entry, true
	}

	models, err := f.Catalog(ctx, actingUserId)
	if err != nil {
		entry.err = err
		return entry, true
	}

	// A SELECTOR IS AVAILABLE WHEN ANY MODEL IS ONLINE, and the concrete model
	// is chosen later, in call(), once the needs are known.
	//
	// Deciding it here would mean deciding it without them, and a fleet
	// running one structured-capable model and one embeddings model would
	// answer "unavailable" for whichever the entry happened to pick -- for a
	// call the other one could have served. Availability here answers "is
	// there any local model at all", which is exactly the question the chain
	// walk is asking.
	//
	// The ROUTER does not reach this path: it expands a selector into concrete
	// model ids through FleetCandidatesFor, because a decision record has to
	// name a model and because only the request knows the context-window floor.
	// This path serves the callers that still resolve a provider by name.
	if isSelector {
		for _, m := range models {
			if m.Online() {
				entry.Available = true
				return entry, true
			}
		}
		entry.err = fmt.Errorf("no local model is online")
		return entry, true
	}

	for _, m := range models {
		if m.ModelId != modelId {
			continue
		}
		if !m.Online() {
			entry.err = fmt.Errorf("no machine offering %s is online", modelId)
			return entry, true
		}
		entry.Available = true
		client.attributes = m
		return entry, true
	}
	entry.err = fmt.Errorf("no machine offers %s", modelId)
	return entry, true
}

// actingUserFromContext resolves whose machines a call may use.
//
// A context with no access carries no acting user, which is SYSTEM WORK. That
// reading is safe in the one direction that matters: system work reaches only
// opted-in machines, so an unresolved user narrows the fleet rather than
// widening it. The opposite default -- treating an unknown caller as some
// user -- would be the cross-user routing memql#4678 exists to prevent.
func actingUserFromContext(ctx context.Context) string {
	access, ok := auth.AccessFromContext(ctx)
	if !ok || access == nil {
		return ""
	}
	return strings.TrimSpace(access.UserId)
}

// fleetProvider is the client behind a `fleet:<modelId>` entry. It implements
// the chat, structured, streaming and embedding surfaces by dispatching
// through the installed FleetInference.
type fleetProvider struct {
	registry *ProviderRegistry
	modelId  string
	// actingUserId is whoever the ENTRY was resolved for. It wins over the
	// call context so availability and dispatch cannot disagree about whose
	// machines are eligible -- a disagreement in that direction is a silent
	// cloud call for a user whose laptop was awake.
	actingUserId string
	// selector is FleetSelectorStrongest or FleetSelectorFastest for a
	// provider whose concrete model is chosen per call rather than at entry
	// time, and empty for one pinned to a model id.
	selector         string
	attributes       FleetModel
	minContextTokens int
	// lastMu guards the surface bookkeeping the ledger reads back after a
	// call. It is per-entry rather than per-call because the provider
	// interfaces return a string and have nowhere to carry it.
	lastMu      sync.Mutex
	lastSurface string
	// lastMachineOwner is the surface bookkeeping's second field, beside
	// lastSurface and for its reason: the provider interfaces return a
	// string, so anything the decision row needs has to be read back rather
	// than returned (epic memql#5327, design D15).
	lastMachineOwner string
	lastUsage        FleetUsage
}

// LastCall reports the machine and usage of this provider's most recent call.
// The provider interfaces return a bare string, so the accounting has to be
// read back rather than returned; memql#4681 stamps the ledger from it.
// ExecutionSurface reports where the last call ran, for the router's decision
// row (epic memql#5146).
//
// It satisfies a structural interface declared in component/router, which is
// its own module and cannot name a method of this one -- see that file for
// why the seam has this shape. The empty string is a real answer: it is what
// this provider says before it has served anything.
func (p *fleetProvider) ExecutionSurface() string {
	if p == nil {
		return ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastSurface
}

// MachineOwner reports whose machine served the last call, and "" when it was
// the caller's own or when nothing has been served (epic memql#5327, D15).
//
// It satisfies a structural interface declared in component/router, beside
// ExecutionSurface and for exactly its reason -- that module pins the root
// module at a published version and cannot name a method of this one. The
// empty string is a real answer here twice over: no call yet, and a call on
// your own hardware.
func (p *fleetProvider) MachineOwner() string {
	if p == nil {
		return ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastMachineOwner
}

func (p *fleetProvider) LastCall() (surface string, usage FleetUsage) {
	if p == nil {
		return "", FleetUsage{}
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastSurface, p.lastUsage
}

func (p *fleetProvider) inference() FleetInference {
	if p == nil || p.registry == nil {
		return nil
	}
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	return p.registry.fleet
}

func (p *fleetProvider) call(ctx context.Context, req FleetCallRequest) (FleetCallResult, error) {
	f := p.inference()
	if f == nil {
		return FleetCallResult{}, fmt.Errorf("%w: this node has no fleet inference installed", ErrFleetUnavailable)
	}
	req.ModelId = p.modelId
	if registrationId := common.FleetRegistrationFromContext(ctx); registrationId != "" {
		req.RegistrationId = registrationId
	}
	req.ActingUserId = p.actingUserId
	if strings.TrimSpace(req.ActingUserId) == "" {
		req.ActingUserId = actingUserFromContext(ctx)
	}

	req.ContextTokens = max(req.ContextTokens, p.minContextTokens)
	if req.Kind == FleetKindChat || req.Kind == FleetKindVision {
		working, err := fleetWorkingContext(req)
		if err != nil {
			return FleetCallResult{}, err
		}
		req.ContextTokens = max(req.ContextTokens, working)
	}

	if p.selector != "" {
		chosen, err := p.resolveSelector(ctx, f, req)
		if err != nil {
			return FleetCallResult{}, err
		}
		req.ModelId = chosen
	}

	// THE GUARDS (memql#4680). This is the one seam every fleet call passes,
	// and it is where the chokepoint moved to: a local call has no
	// *http.Client, so guardedTransport -- which the whole defense-in-depth
	// story was written around -- never sees it. Same four checks, same
	// shared state, zero dollars.
	if err := GuardLocalModelCall(ctx, FleetCallFingerprint(req)); err != nil {
		return FleetCallResult{}, err
	}

	res, err := f.Call(ctx, req)
	if err != nil {
		return res, err
	}
	p.lastMu.Lock()
	p.lastSurface = res.ExecutionSurface
	p.lastMachineOwner = res.MachineOwnerUserId
	p.lastUsage = res.Usage
	p.lastMu.Unlock()
	return res, nil
}

// resolveSelector picks the concrete model a `fleet:strongest` or
// `fleet:fastest` call runs on.
//
// In the selector's order, among the models that can actually serve THIS
// call: the needs come off the request, so a structured turn and an embedding
// turn on the same fleet legitimately land on different models. For
// `strongest`, the owner's modelPreference wins over the size ordering when
// they have one -- it is an explicit statement about their own hardware, and
// the default ordering exists precisely for the users who have not made one.
//
// A miss is the TYPED refusal naming every model considered and why each was
// ruled out, in the same grammar the machine-level refusal uses. "Your fleet
// has nothing that can do this" is the answer, and an operator reading it
// needs to know whether the fix is waking a laptop, pulling a bigger model,
// or using a runtime that supports tools.
func (p *fleetProvider) resolveSelector(ctx context.Context, f FleetInference, req FleetCallRequest) (string, error) {
	models, err := f.Catalog(ctx, req.ActingUserId)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrFleetUnavailable, err)
	}
	var preference []string
	if p.selector == FleetSelectorStrongest {
		if pref, err := f.ModelPreference(ctx, req.ActingUserId); err == nil {
			// A policy read that failed must not decide the model. Falling
			// back to the default ordering is the honest degrade: the caller
			// gets the strongest eligible model rather than a refusal over a
			// row nobody asked about.
			preference = pref
		}
	}
	ordered, err := orderBySelector(models, p.selector, preference)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrFleetUnavailable, err)
	}

	needs := req.Needs()
	considered := map[string]string{}
	for _, m := range ordered {
		if ok, why := m.eligibleForSelector(p.selector, needs); ok {
			return m.ModelId, nil
		} else {
			considered[m.ModelId] = why
		}
	}
	return "", &FleetUnavailable{
		ModelId:    p.selector,
		Considered: considered,
		Total:      len(models),
	}
}

// Call implements AIProvider -- the bare prompt form.
func (p *fleetProvider) Call(ctx context.Context, prompt string) (any, error) {
	res, err := p.call(ctx, FleetCallRequest{
		Kind:     FleetKindChat,
		Messages: []common.ChatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return nil, err
	}
	return res.Content, nil
}

// CallChat implements common.ChatAIProvider.
func (p *fleetProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	res, err := p.call(ctx, FleetCallRequest{Kind: FleetKindChat, Messages: messages})
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// CallChatStructured implements common.ChatStructuredProvider.
//
// The schema is sent to the runtime rather than appended to the prompt. A
// machine only reaches this method if it advertised structured output for the
// model (the router gates on it), so a runtime that quietly returns prose here
// has broken its own advertisement -- and failing is the right answer, because
// the caller is about to parse the reply.
func (p *fleetProvider) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	text, _, err := p.CallChatStructuredWithUsage(ctx, messages, schema)
	return text, err
}

// CallChatStructuredWithUsage keeps the runtime's measured usage on the work
// journal and artifact provenance instead of dropping it at the fleet seam.
func (p *fleetProvider) CallChatStructuredWithUsage(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	res, err := p.call(ctx, FleetCallRequest{Kind: FleetKindChat, Messages: messages, Schema: &schema})
	return res.Content, common.ChatUsage{InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens, Model: res.Usage.Model, Reported: res.Usage.Known}, err
}

// CallChatWithTools implements common.ToolCallingChatAIProvider -- the
// non-streaming tool-calling surface the background execution lane uses
// (design D11).
//
// The tool schemas are sent to the RUNTIME rather than described in the
// prompt. A machine reaches this method only because it advertised `tools=1`
// for the model (the router's capability gate), so a runtime that quietly
// answers prose here has broken its own advertisement -- and the caller is
// about to look for tool calls, so failing to find them is the honest result
// rather than something to paper over with a prose parser.
//
// InputSchema is marshalled ONCE here rather than at each hop: the wire
// carries the schema as a string, and re-encoding it per machine would give
// two candidates for the same call two different schema bytes.
func (p *fleetProvider) CallChatWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (*common.ToolCallingChatResult, error) {
	res, err := p.call(ctx, FleetCallRequest{
		Kind:     FleetKindChat,
		Messages: messages,
		Tools:    tools,
	})
	if err != nil {
		return nil, err
	}
	return &common.ToolCallingChatResult{
		AssistantText: res.Content,
		ToolCalls:     res.ToolCalls,
	}, nil
}

// CallChatStreamWithTools implements common.ChatStreamWithToolsProvider.
//
// The stream carries TEXT incrementally and the tool calls at the END, which
// is what the two runtimes actually do: an OpenAI-compatible endpoint streams
// tool arguments in fragments the worker reassembles, and Ollama emits its
// tool-call list complete on the final message. Rather than inventing a
// synthetic per-fragment delta that only one runtime could ever produce
// faithfully, the assembled calls are emitted once, on the closing chunk --
// the same list CallChatWithTools would have returned.
func (p *fleetProvider) CallChatStreamWithTools(
	ctx context.Context,
	messages []common.ChatMessage,
	tools []common.ToolDefinition,
) (<-chan common.StreamToolChunk, error) {
	out := make(chan common.StreamToolChunk, 32)
	go func() {
		defer close(out)
		res, err := p.call(ctx, FleetCallRequest{
			Kind:     FleetKindChat,
			Messages: messages,
			Tools:    tools,
			OnDelta: func(s string) {
				select {
				case out <- common.StreamToolChunk{Content: s}:
				case <-ctx.Done():
				}
			},
		})
		if err != nil {
			select {
			case out <- common.StreamToolChunk{Error: err, Done: true}:
			case <-ctx.Done():
			}
			return
		}
		final := common.StreamToolChunk{Done: true}
		for i, c := range res.ToolCalls {
			final.ToolCalls = append(final.ToolCalls, common.ToolCallDelta{
				Index:     i,
				ID:        c.ID,
				Name:      c.Name,
				Arguments: c.Arguments,
			})
		}
		select {
		case out <- final:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// CallChatStream implements common.ChatStreamProvider.
func (p *fleetProvider) CallChatStream(ctx context.Context, messages []common.ChatMessage) (<-chan common.StreamChunk, error) {
	out := make(chan common.StreamChunk, 32)
	go func() {
		defer close(out)
		_, err := p.call(ctx, FleetCallRequest{
			Kind:     FleetKindChat,
			Messages: messages,
			OnDelta: func(s string) {
				select {
				case out <- common.StreamChunk{Content: s}:
				case <-ctx.Done():
				}
			},
		})
		if err != nil {
			select {
			case out <- common.StreamChunk{Error: err, Done: true}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case out <- common.StreamChunk{Done: true}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// Embed implements EmbeddingAIProvider.
func (p *fleetProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	vs, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("fleet embed: the runtime returned no vector")
	}
	return vs[0], nil
}

// EmbedBatch implements EmbeddingAIProvider.
func (p *fleetProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	res, err := p.call(ctx, FleetCallRequest{Kind: FleetKindEmbedding, EmbeddingInput: texts})
	if err != nil {
		return nil, err
	}
	if len(res.Embeddings) != len(texts) {
		// A vector count that does not match the input count cannot be
		// matched back up, and guessing the alignment would attach the wrong
		// meaning to a row that then looks correct forever.
		return nil, fmt.Errorf("fleet embed: asked for %d vectors, the runtime returned %d", len(texts), len(res.Embeddings))
	}
	return res.Embeddings, nil
}

// Dimensions implements EmbeddingAIProvider.
//
// IT ASKS THE CATALOG NOW (epic memql#5137, D6), where before it returned 0 and
// said zero was the honest answer. That was true and is no longer the whole
// story: a runtime still reports its vector length only by producing one, but
// the CATALOG records it -- `dimensions` on the `v1:models:modelProfile` row for
// this exact model id -- because the embedder binding cannot create its
// `node_vectors_<dims>` table without knowing the width before the first vector
// exists.
//
// Zero is still what an unknown model gets, and callers still read zero as
// "unknown" rather than as a dimensionality. The difference is that a model the
// catalog knows can now be BOUND, and one it does not cannot -- which is a
// better failure than a table created at the wrong width, because a mismatched
// vector column is not an error anywhere. It is a search space that quietly
// returns the wrong neighbours.
func (p *fleetProvider) Dimensions() int {
	if p == nil {
		return 0
	}
	if dims, ok := catalogDimensionsFor(p.modelId); ok {
		return dims
	}
	return 0
}

var (
	_ AIProvider                         = (*fleetProvider)(nil)
	_ common.ChatAIProvider              = (*fleetProvider)(nil)
	_ common.ChatStructuredProvider      = (*fleetProvider)(nil)
	_ common.ChatStreamProvider          = (*fleetProvider)(nil)
	_ common.ToolCallingChatAIProvider   = (*fleetProvider)(nil)
	_ common.ChatStreamWithToolsProvider = (*fleetProvider)(nil)
	_ common.VisionAIProvider            = (*fleetProvider)(nil)
	_ EmbeddingAIProvider                = (*fleetProvider)(nil)
)
