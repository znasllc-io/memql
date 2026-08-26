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

// Model call kinds, mirroring the wire.
const (
	FleetKindChat      = "chat"
	FleetKindEmbedding = "embedding"
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
	ModelId          string
	ContextWindow    int
	StructuredOutput bool
	Embeddings       bool
	Machines         []FleetMachine
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
	MinContextWindow int
}

// FleetCallRequest is one model call.
type FleetCallRequest struct {
	// ActingUserId scopes the call to that user's machines. EMPTY MEANS
	// SYSTEM WORK, which reaches only machines whose owner opted in -- it
	// does not mean "any machine". The two paths are separate all the way
	// down (memql#4678).
	ActingUserId string
	ModelId      string
	Kind         string
	Messages     []common.ChatMessage
	// Schema is set for a structured call; its presence is also what makes
	// the call require a structured-output-capable model.
	Schema         *common.StructuredSchema
	EmbeddingInput []string
	Purpose        string
	PlanId         string
	TaskId         string
	// OnDelta, when set, receives streamed content as it arrives.
	OnDelta func(string)
}

// Needs derives the capability requirement from the request itself, so a
// caller cannot ask for structured output and forget to require a model that
// can do it.
func (r FleetCallRequest) Needs() FleetNeeds {
	return FleetNeeds{
		StructuredOutput: r.Schema != nil,
		Embeddings:       r.Kind == FleetKindEmbedding,
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
	Usage      FleetUsage
	// ExecutionSurface names the machine that served the call, in the
	// `fleet:<registrationId>` form the ledger stores (memql#4681).
	ExecutionSurface string
	// MachineLabel is the human-readable name for a card or a log line.
	MachineLabel string
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
	f := r.fleet
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

// IsFleetReference reports whether a policy's provider name refers to a fleet
// model, and returns the model id.
func IsFleetReference(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, FleetReferencePrefix) {
		return "", false
	}
	modelId := strings.TrimSpace(strings.TrimPrefix(name, FleetReferencePrefix))
	return modelId, modelId != ""
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
func (r *ProviderRegistry) fleetEntry(ctx context.Context, modelId string) (*ProviderConfigEntry, bool) {
	r.mu.RLock()
	f := r.fleet
	r.mu.RUnlock()

	cfg := ProviderConfig{
		Name:  FleetReferencePrefix + modelId,
		Type:  FleetProviderType,
		Model: modelId,
	}
	client := &fleetProvider{registry: r, modelId: modelId}
	entry := &ProviderConfigEntry{Config: cfg, Client: client}
	if f == nil {
		// No worker service on this node. UNAVAILABLE, not an error: the
		// authored fallback runs, or the work parks.
		entry.err = fmt.Errorf("this node has no fleet inference installed")
		return entry, true
	}

	actingUserId := actingUserFromContext(ctx)
	models, err := f.Catalog(ctx, actingUserId)
	if err != nil {
		entry.err = err
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
	registry   *ProviderRegistry
	modelId    string
	attributes FleetModel
	// lastMu guards the surface bookkeeping the ledger reads back after a
	// call. It is per-entry rather than per-call because the provider
	// interfaces return a string and have nowhere to carry it.
	lastMu      sync.Mutex
	lastSurface string
	lastUsage   FleetUsage
}

// LastCall reports the machine and usage of this provider's most recent call.
// The provider interfaces return a bare string, so the accounting has to be
// read back rather than returned; memql#4681 stamps the ledger from it.
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
	req.ActingUserId = actingUserFromContext(ctx)

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
	p.lastUsage = res.Usage
	p.lastMu.Unlock()
	return res, nil
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
	res, err := p.call(ctx, FleetCallRequest{Kind: FleetKindChat, Messages: messages, Schema: &schema})
	if err != nil {
		return "", err
	}
	return res.Content, nil
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
// ZERO IS THE HONEST ANSWER for a local model, and callers treat it as
// "unknown" rather than as a dimensionality. Every runtime in the v1 set
// (Ollama, OpenAI-compatible endpoints) reports the vector length only by
// producing one, and inventing a number here would be a claim that survives
// long after the model behind it was swapped.
func (p *fleetProvider) Dimensions() int { return 0 }

var (
	_ AIProvider                    = (*fleetProvider)(nil)
	_ common.ChatAIProvider         = (*fleetProvider)(nil)
	_ common.ChatStructuredProvider = (*fleetProvider)(nil)
	_ common.ChatStreamProvider     = (*fleetProvider)(nil)
	_ EmbeddingAIProvider           = (*fleetProvider)(nil)
)
