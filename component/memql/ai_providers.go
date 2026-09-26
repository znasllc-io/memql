package memql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"github.com/znasllc-io/memql/core/audio"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/num"
)

// VarDefaultTTSProvider is the ONE default-provider variable left, and it is
// left because text-to-speech has no level yet (epic memql#5137, D3).
//
// MEMQL_DEFAULT_PROVIDER, MEMQL_DEFAULT_CHAT_PROVIDER and
// MEMQL_DEFAULT_STREAM_PROVIDER are deleted. Each named a vendor model in a
// deployment manifest, which put a routing decision somewhere no rule could see
// and no decision record could explain -- and since every concrete record is a
// paid model, each was a paid default with an operator's name on it. Chat and
// streaming resolve through the router at a LEVEL now.
//
// Speech does not, yet. The fleet gained a speak door in memql#5141, but the
// four levels are chat levels, so a TTS call still names a provider. When the
// speech door has a level of its own this goes the way of the other three.
const VarDefaultTTSProvider = "MEMQL_DEFAULT_TTS_PROVIDER"

// ProviderModality defines the input/output mode of a provider.
type ProviderModality string

const (
	ModalityText      ProviderModality = "text"      // Text input → text output (chat, stream)
	ModalityTTS       ProviderModality = "tts"       // Text input → audio output (text-to-speech)
	ModalitySTT       ProviderModality = "stt"       // Audio input → text output (speech-to-text)
	ModalityEmbedding ProviderModality = "embedding" // Text input → vector output (embeddings)
)

// THE OTHER EIGHT MODALITIES ARE GONE, and their absence is the point (epic
// memql#5137, D3).
//
// realtime, audio, image, video, computerUse, moderation, search and research
// were declared here so a provider .memql file could "register intent", and
// eleven records did. Every one of them routed through
// newOpenAIPlaceholderProvider: a client that validated the credential at
// registration, reported itself AVAILABLE, and returned "the Go client is not
// wired yet" to any call that reached it.
//
// That is worse than an absence. A registered, available provider is one a
// policy can name, a page can list and a router can pick -- so the failure
// arrived at call time, three layers from the record that promised the
// capability, in a cluster where the operator had every reason to believe the
// door was open.
//
// The four modalities the fleet wire now serves locally (vision, transcribe,
// speak, image -- memql#5141) are NOT re-declarations of these. They are call
// KINDS on WorkerService.Stream, served by a machine that advertised the
// capability, and a machine that advertises nothing is skipped during
// selection rather than picked and then failed.

// ProviderConfig captures serialized provider metadata.
type ProviderConfig struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Model       string            `json:"model"`
	Auth        map[string]string `json:"auth"`
	Params      map[string]any    `json:"params"`
	Default     bool              `json:"default,omitempty"`
	Modality    string            `json:"modality,omitempty"` // "text" or "audio"; inferred from Type if not set
	Description string            `json:"description,omitempty"`
	Base        bool              `json:"-"` // true for base provider definitions (not registered as providers)
	Extends     string            `json:"-"` // name of base provider to inherit auth/type from
	Disabled    bool              `json:"-"` // @disabled lifecycle flag: skipped at load (not registered, no auth resolution); on a @base it propagates to @extends children
}

// ContextWindow returns the context window size from provider params,
// falling back to 0 if not explicitly set. Callers should use
// polyphon.LookupContextWindow(model) as a fallback for unknown values.
//
// SATURATES out of range (memql#4779). The value is a capacity, reported into
// the router's decision payload, and a negative capacity would make every
// prompt look too long for the provider that declared it.
func (c ProviderConfig) ContextWindow() int {
	if cw, ok := c.Params["contextWindow"]; ok {
		switch v := cw.(type) {
		case float64:
			return num.ClampFloat64(v)
		case int:
			return v
		}
	}
	return 0
}

// Streaming reports whether this record's model should be served by the
// streaming client (epic memql#5137, D3).
//
// IT IS A CAPABILITY, NOT A TYPE, and that is the whole point of the field.
// Every vendor model used to carry TWO records -- `chat54Mini` and
// `stream54Mini`, one @vendor("OpenAI") and one @vendor("OpenAIStream") -- for one
// model with one price. The two drifted, exactly as a duplicated fact does:
// they disagreed about gpt-5.4-mini's maxCompletionTokens, and the streaming
// copy was the one the agent reply path actually used, so it was the copy that
// was wrong more often.
//
// The collapse is safe because the streaming clients are a strict SUPERSET:
// openAIStreamProvider and anthropicStreamProvider implement Call, CallChat and
// CallChatWithTools alongside the three streaming methods, so a record that
// declares `streaming true` can still serve every non-streaming caller.
//
// Absent reads as false, which is the fail-closed direction: a model whose
// record says nothing gets the plain client, and a caller wanting a stream from
// it gets a typed "provider does not stream" rather than a silent buffer of the
// whole completion.
func (c ProviderConfig) Streaming() bool {
	switch v := c.Params["streaming"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// ResolvedModality returns the effective modality, inferring from Type if not explicitly set.
func (c ProviderConfig) ResolvedModality() ProviderModality {
	if c.Modality != "" {
		return ProviderModality(strings.ToLower(c.Modality))
	}
	// Infer from provider type. Four types, four modalities: the eight that
	// inferred a placeholder modality went with the placeholder client (D3).
	// An unrecognised type reads as text, which is what it read as before and
	// is the right default -- a chat record is by far the common case, and a
	// modality guessed wrong here would take a text call out of the text pool.
	switch strings.ToLower(c.Type) {
	case "openaitts":
		return ModalityTTS
	case "openaistt", "openaiwhisper":
		return ModalitySTT
	case "openaiembedding":
		return ModalityEmbedding
	default:
		return ModalityText
	}
}

// SupportsText returns true if this provider can be used for text-based ai() calls.
func (c ProviderConfig) SupportsText() bool {
	return c.ResolvedModality() == ModalityText
}

// Pricing captures the per-million-token USD prices declared by a provider's
// .memql file. Zero values mean "not yet configured" -- callers should treat
// that as unknown cost, not free.
type Pricing struct {
	InputPerMillion       float64
	OutputPerMillion      float64
	CachedInputPerMillion float64
}

// Pricing reads inputCostPerMillion / outputCostPerMillion /
// cachedInputCostPerMillion from provider params. Values are USD per
// million tokens and are set via params in the provider .memql file.
func (c ProviderConfig) Pricing() Pricing {
	return Pricing{
		InputPerMillion:       floatParam(c.Params, "inputCostPerMillion"),
		OutputPerMillion:      floatParam(c.Params, "outputCostPerMillion"),
		CachedInputPerMillion: floatParam(c.Params, "cachedInputCostPerMillion"),
	}
}

// Configured reports whether any pricing was declared for this provider.
// Used to decide whether router call rows get marked pricingKnown=false.
func (p Pricing) Configured() bool {
	return p.InputPerMillion > 0 || p.OutputPerMillion > 0 || p.CachedInputPerMillion > 0
}

// CostFor returns the USD cost breakdown for a call with the given token
// counts. The cached-input portion is billed at the cached rate and the
// remaining input at the regular rate.
func (p Pricing) CostFor(inputTokens, outputTokens, cachedInputTokens int) (input, output, cachedInput, total float64) {
	regularInput := inputTokens - cachedInputTokens
	if regularInput < 0 {
		regularInput = 0
	}
	input = p.InputPerMillion * float64(regularInput) / 1_000_000
	cachedInput = p.CachedInputPerMillion * float64(cachedInputTokens) / 1_000_000
	output = p.OutputPerMillion * float64(outputTokens) / 1_000_000
	total = input + output + cachedInput
	return
}

func floatParam(params map[string]any, key string) float64 {
	if params == nil {
		return 0
	}
	v, ok := params[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return 0
}

// AIProvider executes prompts against a backing model.
type AIProvider interface {
	Call(ctx context.Context, prompt string) (any, error)
}

// StreamingAIProvider extends AIProvider with streaming support.
type StreamingAIProvider interface {
	AIProvider
	CallStream(ctx context.Context, prompt string) (<-chan StreamChunk, error)
}

// RealtimeAIProvider extends AIProvider for WebSocket-based realtime interactions.
type RealtimeAIProvider interface {
	AIProvider
	// Connect establishes a WebSocket connection to the Realtime API
	Connect(ctx context.Context) (RealtimeSession, error)
	// CreateClientSecret creates an ephemeral client secret for browser-based WebRTC.
	// This calls OpenAI's /realtime/sessions REST endpoint (not WebSocket).
	CreateClientSecret(ctx context.Context) (*RealtimeClientSecretResponse, error)
	// SessionConfig returns the session configuration for this provider.
	SessionConfig() RealtimeSessionConfig
}

// RealtimeClientSecretResponse contains the ephemeral client secret for WebRTC sessions.
type RealtimeClientSecretResponse struct {
	Value     string `json:"value"`
	ExpiresAt any    `json:"expiresAt"`
}

// RealtimeSessionConfig describes the session configuration for a realtime provider.
type RealtimeSessionConfig struct {
	Model              string         `json:"model"`
	Voice              string         `json:"voice"`
	TurnDetection      map[string]any `json:"turnDetection,omitempty"`
	Instructions       string         `json:"instructions,omitempty"`
	TranscriptionModel string         `json:"transcriptionModel,omitempty"`
}

// TTSAIProvider provides text-to-speech synthesis capabilities.
type TTSAIProvider interface {
	AIProvider
	// Synthesize converts text to audio and returns the raw audio bytes.
	Synthesize(ctx context.Context, text string, voice string) ([]byte, error)
	// SynthesizeStream converts text to audio and streams chunks via channel.
	SynthesizeStream(ctx context.Context, text string, voice string) (<-chan TTSChunk, error)
}

// TTSChunk represents a chunk of synthesized audio.
type TTSChunk struct {
	Audio    []byte // Raw audio data
	Sequence int    // Chunk sequence number (starts at 0)
	Done     bool   // True for the final chunk
	Error    error  // Non-nil if an error occurred
}

// StreamChunk represents a single chunk from a streaming response.
type StreamChunk struct {
	Content string
	Done    bool
	Error   error
}

// RealtimeSession represents an active Realtime API connection.
type RealtimeSession interface {
	// SendAudio sends audio data to the model (base64 encoded PCM16)
	SendAudio(ctx context.Context, audio []byte) error
	// SendText sends text input to the model
	SendText(ctx context.Context, text string) error
	// Receive returns a channel of events from the model
	Receive() <-chan RealtimeEvent
	// Close terminates the session
	Close() error
}

// RealtimeEvent represents an event from the Realtime API.
type RealtimeEvent struct {
	Type    RealtimeEventType
	Audio   []byte // For audio.delta events (base64 encoded)
	Text    string // For text responses
	Error   error
	RawJSON json.RawMessage
}

// RealtimeEventType categorizes events from the Realtime API.
type RealtimeEventType string

const (
	RealtimeEventAudioDelta    RealtimeEventType = "response.audio.delta"
	RealtimeEventAudioDone     RealtimeEventType = "response.audio.done"
	RealtimeEventTextDelta     RealtimeEventType = "response.text.delta"
	RealtimeEventTextDone      RealtimeEventType = "response.text.done"
	RealtimeEventError         RealtimeEventType = "error"
	RealtimeEventSessionUpdate RealtimeEventType = "session.update"
)

// ProviderRegistry tracks configured providers and their availability.
type ProviderRegistry struct {
	mu     sync.RWMutex
	byName map[string]*ProviderConfigEntry
	// declared holds every provider NAME the DSL tree declares, including
	// the @disabled ones that never become registry entries. The two sets
	// differ on purpose and the difference is load-bearing: "@disabled, so
	// dependents fall back" is a documented lifecycle state (#1081), while
	// "named nowhere in the tree" is a typo. Only the second is a load
	// failure -- see ValidatePromptDefaultProviders.
	declared map[string]bool
	// fleet is the local-model seam (epic memql#4676). Nil on every build
	// with no worker service, which is an UNAVAILABLE fleet rather than a
	// broken one -- see fleet_provider.go.
	fleet        FleetInference
	fleetCatalog FleetCatalogReader
	// apps is the subscription-app door seam (epic memql#5096). Nil for the
	// same reason and with the same meaning: a node with no worker service
	// has no app door, which is a state the chain walks past rather than an
	// error it raises -- see app_provider.go.
	apps AppInference
	// appSessions is the STEP-handover seam (epic memql#5391, design D7):
	// how a tool-needing call resolved to an app door becomes a session
	// subrun. Nil on a build with no worker service, and unlike `apps` a nil
	// here REFUSES rather than being walked past -- the router already chose
	// this door, so there is no chain left to continue. See
	// app_session_provider.go.
	appSessions AppSessionDelegate
}

// ProviderConfigEntry stores metadata + instantiated client for a provider.
type ProviderConfigEntry struct {
	Config    ProviderConfig
	Client    AIProvider
	Available bool
	err       error
}

// Err reports why an entry is unavailable, or nil when it is not.
//
// The field is unexported so nothing outside this package can SET it; the
// reason is worth reading, because "no machine offering llama3.1:8b is
// online" and "this node has no fleet inference installed" are the same
// Available=false with entirely different fixes.
func (e *ProviderConfigEntry) Err() error {
	if e == nil {
		return nil
	}
	return e.err
}

// newProviderRegistry builds an empty registry.
//
// It TOOK a default provider name, read from MEMQL_DEFAULT_PROVIDER, and takes
// none now (epic memql#5137, D3). A provider named by an environment variable
// is a routing decision made in a deployment manifest: nothing in the graph
// records it, no decision record can name it, and an operator reading the rules
// to find out why a paid model answered would find nothing that says so.
func newProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		byName:   make(map[string]*ProviderConfigEntry),
		declared: make(map[string]bool),
	}
}

// markDeclared records that the DSL tree declares a provider by this name,
// whether or not it ends up registered (a @disabled provider is declared
// but never registered).
func (r *ProviderRegistry) markDeclared(name string) {
	if r == nil {
		return
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.declared == nil {
		r.declared = make(map[string]bool)
	}
	r.declared[key] = true
}

// Declared reports whether the DSL tree declares a provider by this name.
// True for registered providers AND for @disabled ones -- the question it
// answers is "does this name exist?", not "can it serve a call right now?".
func (r *ProviderRegistry) Declared(name string) bool {
	if r == nil {
		return false
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.declared[key] {
		return true
	}
	// A registry seeded by a path that predates the declared set (tests
	// that call setEntry directly) still answers honestly.
	_, ok := r.byName[key]
	return ok
}

func (r *ProviderRegistry) setEntry(entry *ProviderConfigEntry) {
	if r == nil || entry == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[strings.TrimSpace(entry.Config.Name)] = entry

	// THE REGISTRY HAS NO DEFAULT (epic memql#5137, D3), and this function
	// used to be where one was chosen. It carried three tiers -- an env pin,
	// a record's @default, and "first available registered provider" -- and all
	// three are gone.
	//
	// The third is the one worth explaining, because it looked harmless. Every
	// concrete record in dsl/providers/providers.memql is a paid vendor model,
	// so "first available" meant: on any cluster with federation configured,
	// the alphabetically-first vendor record silently became the answer to
	// every call that named nothing. No policy said so, no rule said so, and
	// the decision record for the resulting call could not say who chose,
	// because nothing did.
	//
	// What replaces it is the shipped `default` RULE (epic memql#5127): a
	// declaration, in the tree, that says local first and paid last, which a
	// person can read and override with a rule of their own.
}

// adoptContents replaces everything this registry holds with the contents of
// `next`, under this registry's own write lock (epic memql#4440, design D5).
//
// THE SWAP IS THE POINT, and swapping CONTENTS rather than the pointer is what
// makes it safe. `MemQLEngine.providers` is read from ~57 places with no lock
// -- correctly, because the field is written once at boot and never again --
// so a reload that reassigned the field would be a data race against every one
// of them, and rebuilding aiRuntime to match would silently drop the semantic
// cache that SetSemanticCache attached to the old one.
//
// Swapping the map under `mu` instead means every reader keeps the same
// *ProviderRegistry and every accessor already takes RLock, so an in-flight
// call sees either the whole old set or the whole new one and never a mixture.
// That retires the "the swap is non-atomic across providers" caveat that
// ReloadAIProviders carried, rather than inheriting it.
//
// `next` MUST be fully built before this is called -- auth resolved, clients
// constructed. Building inside the lock would hold it across network-capable
// constructors and stall every reader for the duration.
func (r *ProviderRegistry) adoptContents(next *ProviderRegistry) {
	if r == nil || next == nil {
		return
	}
	// Read the incoming side under ITS lock: it is a local value here, but
	// taking the lock keeps this correct if a caller ever shares one.
	next.mu.RLock()
	byName := make(map[string]*ProviderConfigEntry, len(next.byName))
	for k, v := range next.byName {
		byName[k] = v
	}
	declared := make(map[string]bool, len(next.declared))
	for k, v := range next.declared {
		declared[k] = v
	}
	next.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName = byName
	r.declared = declared
}

// AvailableCount returns how many REGISTERED entries are callable.
//
// Distinct from Count(), which counts everything registered including the
// @base entries that are Available=false on purpose and the unavailable ones a
// keyless cluster is full of. A reload reporting Count() would say "12
// providers" on a node where none of them work, which is the number an
// operator would read as success.
func (r *ProviderRegistry) AvailableCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, entry := range r.byName {
		if entry != nil && entry.Available {
			n++
		}
	}
	return n
}

// Count returns the number of registered providers.
func (r *ProviderRegistry) Count() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// Names returns every REGISTERED provider name, sorted.
//
// Registered, not declared: a @disabled provider is deliberately never
// registered and never resolves its auth, so it is not something this node has
// loaded. `declared` is the separate set, and the difference between the two is
// load-bearing (see the field comment on it).
func (r *ProviderRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// FederatedByStrength lists the available federated providers, strongest first.
//
// IT IS NOT A DEFAULT, and the distinction is the whole reason it may exist at
// all (epic memql#5137, D3). Nothing reaches it without an explicit human
// consent on the call in front of them; there is no path where the platform
// picks from this list on its own. What it exists for is the case the consent
// path has to survive: a cluster whose rule and policy corpus failed to load has
// no `federationStrongest` to resolve through, and consent is precisely the
// escape for when the ordinary chain already refused -- so making the escape
// depend on the corpus would leave a person saying yes and nothing happening.
//
// ORDERED BY DECLARED CONTEXT WINDOW, TIE-BROKEN BY NAME, and both halves are
// deliberate. Context window is the one capability every chat record declares,
// so it is the only ordering available without epic memql#5146's measurements;
// the name tie-break is what makes two replicas choose identically and a person
// able to predict the answer. Map order -- which is what the deleted default
// actually was -- would fail both.
func (r *ProviderRegistry) FederatedByStrength() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	type entry struct {
		name   string
		window int
	}
	var candidates []entry
	for name, e := range r.byName {
		if e == nil || !e.Available || e.Config.Base {
			continue
		}
		// Fleet and app doors are not what a CLOUD consent is about: the fleet
		// entry is the one that was unavailable when the consent was asked for,
		// and a subscription app is a door the chain tries before federation.
		switch strings.ToLower(e.Config.Type) {
		case strings.ToLower(FleetProviderType), strings.ToLower(AppProviderType):
			continue
		}
		if e.Config.ResolvedModality() != ModalityText {
			continue
		}
		candidates = append(candidates, entry{name: name, window: e.Config.ContextWindow()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].window != candidates[j].window {
			return candidates[i].window > candidates[j].window
		}
		return candidates[i].name < candidates[j].name
	})
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.name)
	}
	return out
}

// THE REGISTRY HAS NO Default() ANY MORE (epic memql#5137, D3).
//
// It returned the provider name an env var pinned, or a record's @default, or
// -- when neither applied, which was the ordinary case -- whichever available
// entry happened to be reached first. Every concrete record in
// dsl/providers/providers.memql is a paid vendor model, so on any cluster with
// federation configured that last tier silently made one of them the answer to
// every call that named nothing, with no rule, no policy and no decision record
// able to say who chose.
//
// Callers that used to fall back to it now REFUSE, and the refusal names what
// is missing. Under epic memql#5127 that refusal is unreachable in practice --
// the shipped `default` rule supplies a chain before any caller gets there --
// which is the correct shape: the default is a declaration in the tree, not a
// property of a map.

// Entry retrieves a provider configuration entry by name.
//
// A `fleet:<modelId>` name is resolved DYNAMICALLY against the live catalog
// (epic memql#4676). It is done here rather than at load because a fleet model
// exists while a laptop is awake: resolving at load would refuse boot on an
// asleep fleet, and a machine waking up would need a reload to become usable.
// Every accessor in this file goes through Entry, so the fleet reaches the
// whole policy-chain machinery with no second lookup path -- and an
// unavailable fleet model behaves exactly like a disabled provider, which is
// what makes an authored @fallback fire without a special case.
func (r *ProviderRegistry) Entry(name string) (*ProviderConfigEntry, bool) {
	return r.EntryForContext(context.Background(), name)
}

// EntryForContext is Entry with the call's context, which is what a fleet
// lookup needs: the acting user decides whose machines are eligible.
func (r *ProviderRegistry) EntryForContext(ctx context.Context, name string) (*ProviderConfigEntry, bool) {
	return r.EntryForUser(ctx, "", name)
}

// EntryForUser is EntryForContext with the acting user stated explicitly.
//
// The AI router carries the user on its ResolveRequest rather than on a
// context, and the distinction is not cosmetic here. Resolving a user's fleet
// model against the SYSTEM catalog would report it unavailable, and an
// unavailable primary with an authored @fallback runs the fallback -- so the
// mismatch would present as a silent cloud call for a user whose laptop was
// awake the whole time. An explicit non-empty userId therefore wins over
// whatever the context says; empty falls back to the context, and only then
// to system work.
func (r *ProviderRegistry) EntryForUser(ctx context.Context, actingUserId, name string) (*ProviderConfigEntry, bool) {
	if r == nil {
		return nil, false
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, false
	}
	r.mu.RLock()
	entry, ok := r.byName[key]
	r.mu.RUnlock()
	if ok {
		return entry, true
	}
	if modelId, isFleet := IsFleetReference(key); isFleet {
		if strings.TrimSpace(actingUserId) == "" {
			actingUserId = actingUserFromContext(ctx)
		}
		return r.fleetEntry(ctx, actingUserId, modelId)
	}
	if appId, model, isApp := SplitAppReference(key); isApp {
		if strings.TrimSpace(actingUserId) == "" {
			actingUserId = actingUserFromContext(ctx)
		}
		return r.appEntry(ctx, actingUserId, appId, model, key)
	}
	return nil, false
}

// providerByName looks up an available named entry, applies an optional
// extra predicate, and type-asserts the Client to T. The {*T, false}
// zero-form covers every "no fit" path: name missing, entry
// unavailable, predicate rejected, or type assertion failed. Used by
// every *ProviderByName accessor.
//
// It resolves through Entry, i.e. against the SYSTEM catalog for a dynamic
// name. Use providerByNameForContext when the caller has one: a `fleet:` or
// `app:` entry resolved without the acting user reports a live machine as
// unavailable, and an unavailable primary with any fallback is a silent cloud
// call for somebody whose laptop was awake the whole time.
func providerByName[T any](r *ProviderRegistry, name string, extra func(*ProviderConfigEntry) bool) (T, bool) {
	return providerByNameForContext[T](context.Background(), r, name, extra)
}

// providerByNameForContext is providerByName with the caller's context, which
// is what a dynamic (`fleet:` / `app:`) name needs to resolve against the
// right machines.
func providerByNameForContext[T any](ctx context.Context, r *ProviderRegistry, name string, extra func(*ProviderConfigEntry) bool) (T, bool) {
	var zero T
	entry, ok := r.EntryForContext(ctx, name)
	if !ok || !entry.Available {
		return zero, false
	}
	if extra != nil && !extra(entry) {
		return zero, false
	}
	v, ok := entry.Client.(T)
	if !ok {
		return zero, false
	}
	return v, true
}

// providerScan iterates the registry looking for the first available
// entry that matches the optional modality filter, the optional extra
// predicate, AND type-asserts to T. Used by every "fall back to first
// available" accessor.
func providerScan[T any](r *ProviderRegistry, modality ProviderModality, extra func(*ProviderConfigEntry) bool) T {
	var zero T
	if r == nil {
		return zero
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, entry := range r.byName {
		if entry == nil || !entry.Available {
			continue
		}
		if modality != "" && entry.Config.ResolvedModality() != modality {
			continue
		}
		if extra != nil && !extra(entry) {
			continue
		}
		if v, ok := entry.Client.(T); ok {
			return v
		}
	}
	return zero
}

// TTSProvider returns a TTS provider. If defaultName is provided and exists, returns that;
// otherwise returns the first available TTS provider, or nil if none configured.
func (r *ProviderRegistry) TTSProvider(defaultName string) TTSAIProvider {
	if r == nil {
		return nil
	}
	if defaultName != "" {
		if tts, ok := r.TTSProviderByName(defaultName); ok {
			return tts
		}
	}
	return providerScan[TTSAIProvider](r, ModalityTTS, nil)
}

// TTSProviderByName returns a specific TTS provider by name.
func (r *ProviderRegistry) TTSProviderByName(name string) (TTSAIProvider, bool) {
	return providerByName[TTSAIProvider](r, name, func(e *ProviderConfigEntry) bool {
		return e.Config.ResolvedModality() == ModalityTTS
	})
}

// ChatProvider returns a non-streaming chat provider suitable for synchronous AI calls
// (e.g., suggest endpoints). If defaultName is provided and exists, returns that;
// otherwise returns the first available non-streaming chat provider, or nil if none configured.
//
// This method explicitly excludes streaming providers (OpenAIStream, AnthropicStream)
// because their CallChat() implementation may target model endpoints that reject
// synchronous chat completions requests. Use StreamProvider() for streaming use cases.
func (r *ProviderRegistry) ChatProvider(defaultName string) common.ChatAIProvider {
	if r == nil {
		return nil
	}

	// Try the specified default first
	if defaultName != "" {
		if entry, ok := r.Entry(defaultName); ok && entry.Available {
			if cp, ok := entry.Client.(common.ChatAIProvider); ok && isNonStreamingType(entry.Config.Type) {
				return cp
			}
		}
	}

	// THE PREFERRED-NAMES LIST IS GONE (epic memql#5137, D3).
	//
	// It was five paid OpenAI records in priority order -- chat54, chat54Mini,
	// chat54Nano, chat54Pro, chat53Latest -- which is a paid default written in
	// Go, the exact shape TestNoPaidDefault's third arm exists to catch.
	//
	// Its stated purpose was real: stop map iteration picking a model that
	// cannot serve /v1/chat/completions. What was wrong was solving that by
	// naming five vendor records rather than by asking whether a record is
	// chat-compatible -- isChatCompatibleModel already answers exactly that
	// question, and the scan below applies it.
	//
	// What the list DID, as opposed to what it was for, was decide silently
	// that every unnamed synchronous call went to OpenAI: on a cluster whose
	// operator had configured only Anthropic it walked five names that were
	// never going to resolve, and on one with both it chose a vendor with
	// nothing in the decision record naming the choice.
	//
	// Iterate all providers, filtering strictly.
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, entry := range r.byName {
		if entry != nil && entry.Available && entry.Config.ResolvedModality() == ModalityText && isNonStreamingType(entry.Config.Type) && isChatCompatibleModel(entry.Config.Model) {
			if cp, ok := entry.Client.(common.ChatAIProvider); ok {
				return cp
			}
		}
	}
	return nil
}

// ChatStructuredProvider returns the first provider that both matches
// the given name (or the default when name is empty) AND implements
// provider-enforced structured output. Returns nil when no structured-
// capable provider is available; callers should fall back to
// ChatProvider + in-prompt schema instructions.
func (r *ProviderRegistry) ChatStructuredProvider(defaultName string) common.ChatStructuredProvider {
	if r == nil {
		return nil
	}
	// Named lookup first.
	name := strings.TrimSpace(defaultName)
	if name != "" {
		if entry, ok := r.Entry(name); ok && entry.Available {
			if cp, ok := entry.Client.(common.ChatStructuredProvider); ok && isNonStreamingType(entry.Config.Type) {
				return cp
			}
		}
	}
	// The five-name paid preference list is gone here too, for the reason
	// recorded in full on ChatProvider above (epic memql#5137, D3).
	//
	// Any structured-capable non-streaming provider.
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, entry := range r.byName {
		if entry == nil || !entry.Available {
			continue
		}
		if cp, ok := entry.Client.(common.ChatStructuredProvider); ok && isNonStreamingType(entry.Config.Type) {
			return cp
		}
	}
	return nil
}

// ChatStructuredProviderByName returns the named provider iff it
// implements ChatStructuredProvider. Unlike ChatStructuredProvider,
// this does NOT fall back to other providers -- caller gets exactly
// the named one or nil. Used when a prompt declares a specific
// structured model.
func (r *ProviderRegistry) ChatStructuredProviderByName(ctx context.Context, name string) common.ChatStructuredProvider {
	if r == nil {
		return nil
	}
	cp, _ := providerByNameForContext[common.ChatStructuredProvider](ctx, r, strings.TrimSpace(name), func(e *ProviderConfigEntry) bool {
		return isNonStreamingType(e.Config.Type)
	})
	return cp
}

// SuggestChatProvider returns a fast, lightweight non-streaming chat provider
// optimized for suggestion endpoints. Prefers smaller/faster models over larger ones.
// Falls back to ChatProvider if no fast model is available.
func (r *ProviderRegistry) SuggestChatProvider() common.ChatAIProvider {
	if r == nil {
		return nil
	}

	// THE THREE FAST NAMES ARE GONE (epic memql#5137, D3), and this one is
	// worth naming separately from ChatProvider's five because it was the same
	// mistake made for a BETTER reason: "prefer a small, cheap model for a
	// suggestion" is exactly right, and encoding it as chat54Nano, chat54Mini,
	// chat54 made it a preference for three specific paid records.
	//
	// The judgement survives as a LEVEL. `fast` is what a suggestion declares
	// (epic memql#5127), and the rules decide which model serves it against
	// what the cluster actually has -- which on a local fleet is a 9B running
	// on somebody's laptop, and was previously nothing at all, because none of
	// the three names could resolve.
	return r.ChatProvider("")
}

// isNonStreamingType returns true for provider types that use synchronous
// (non-streaming) chat completions. These are safe for suggest/non-streaming
// AI calls.
//
// THE LOCAL TYPES BELONG HERE, and Fleet's absence was a silent cloud call
// (epic memql#5096, task memql#5098). The gate exists because a STREAMING
// build may target an endpoint that refuses synchronous completions -- that is
// a fact about two vendor SDKs, not a property of "provider". A fleet call is
// one request and one response over a stream this side already holds, and an
// app answers a prompt in one turn; neither has a streaming-only endpoint to
// be wrong about.
//
// While they were excluded, `ChatStructuredProviderByName("fleet:...")`
// answered nil, and InvokeAIStructured's next step is a registry-wide scan for
// anything structured-capable -- which found a cloud provider the policy never
// named. So a structured prompt with a fleet default was answered by a paid
// API even when the machine was awake. `TestStructuredCallWithAFleetDefault-
// MakesNoCloudCall` is the control that failed before this line changed.
func isNonStreamingType(providerType string) bool {
	switch strings.ToLower(providerType) {
	case "openai", "openaichat", "anthropic", "anthropicchat":
		return true
	case strings.ToLower(FleetProviderType), strings.ToLower(AppProviderType):
		return true
	default:
		return false
	}
}

// isChatCompatibleModel returns true for models known to work with the
// /v1/chat/completions endpoint. Excludes codex models (completion-only)
// and other models that don't support chat.
func isChatCompatibleModel(model string) bool {
	lower := strings.ToLower(model)
	// Codex models use /v1/completions, not /v1/chat/completions
	if strings.Contains(lower, "codex") {
		return false
	}
	// Embedding and moderation models are not chat models
	if strings.Contains(lower, "embedding") || strings.Contains(lower, "moderation") {
		return false
	}
	return true
}

// StreamProvider returns a Streaming provider. If defaultName is provided and exists, returns that;
// otherwise returns the first available Streaming provider, or nil if none configured.
func (r *ProviderRegistry) StreamProvider(defaultName string) StreamingAIProvider {
	if r == nil {
		return nil
	}
	if defaultName != "" {
		if sp, ok := providerByName[StreamingAIProvider](r, defaultName, nil); ok {
			return sp
		}
	}
	return providerScan[StreamingAIProvider](r, ModalityText, nil)
}

// VisionProvider returns the first available provider that implements VisionAIProvider.
// If providerName is provided and exists, returns that; otherwise scans all text providers.
func (r *ProviderRegistry) VisionProvider(providerName string) common.VisionAIProvider {
	if r == nil {
		return nil
	}
	if providerName != "" {
		if vp, ok := providerByName[common.VisionAIProvider](r, providerName, nil); ok {
			return vp
		}
	}
	return providerScan[common.VisionAIProvider](r, ModalityText, nil)
}

// EmbeddingProvider returns a named embedding provider, or the first
// available one if name is empty. The named-lookup path preserves the
// legacy distinction between "not found" and "found but wrong type"
// errors -- callers grep on the error message in a few places.
//
// IT TAKES A CONTEXT because a `fleet:` name resolves against the ACTING
// USER'S machines (epic memql#5096, task memql#5098, design D6). It used to
// read r.byName directly, which is the one map a dynamic name is deliberately
// absent from -- so `fleet:nomic-embed-text` answered "not found" and the
// seeded local embeddings policy had no consumer that could ever have worked.
// The registry lookup below is EntryForContext for exactly that reason, and
// the "not found" branch now also reports whether the fleet said why.
func (r *ProviderRegistry) EmbeddingProvider(ctx context.Context, name string) (EmbeddingAIProvider, error) {
	if r == nil {
		return nil, fmt.Errorf("provider registry is nil")
	}

	if name != "" {
		entry, ok := r.EntryForContext(ctx, name)
		if !ok || entry == nil {
			return nil, fmt.Errorf("embedding provider %q not found", name)
		}
		if !entry.Available {
			// A dynamic entry that resolved but cannot serve. Reported as
			// UNAVAILABLE rather than as "not found": the difference is
			// "your machine is asleep" versus "you spelled it wrong", and
			// they have entirely different fixes.
			return nil, fmt.Errorf("embedding provider %q is unavailable: %v", name, entry.Err())
		}
		ep, ok := entry.Client.(EmbeddingAIProvider)
		if !ok {
			return nil, fmt.Errorf("provider %q is not an embedding provider (type: %s)", name, entry.Config.Type)
		}
		return ep, nil
	}

	if ep := providerScan[EmbeddingAIProvider](r, ModalityEmbedding, nil); ep != nil {
		return ep, nil
	}
	return nil, fmt.Errorf("no embedding provider available")
}

// ProvidersByModality returns all available providers of a given modality.
func (r *ProviderRegistry) ProvidersByModality(modality ProviderModality) []*ProviderConfigEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*ProviderConfigEntry
	for _, entry := range r.byName {
		if entry != nil && entry.Available && entry.Config.ResolvedModality() == modality {
			result = append(result, entry)
		}
	}
	return result
}

// loadAIProviders returns an empty registry. Pass 3 of the DSL
// restructure migration retired the legacy walk over
// dsl/v1/providers/. Providers now load via LoadUnifiedProviders
// (component/memql/unified_kinds_loader.go) which walks
// dsl/providers/<vendor>.memql.
func loadAIProviders(_ *slog.Logger) (*ProviderRegistry, error) {
	return newProviderRegistry(), nil
}

func parseProviderConfigs(origin string, raw []byte) ([]ProviderConfig, error) {
	payload := bytes.TrimSpace(raw)
	if len(payload) == 0 {
		return nil, fmt.Errorf("provider file %q is empty", origin)
	}
	if payload[0] == '[' {
		var list []ProviderConfig
		if err := json.Unmarshal(payload, &list); err != nil {
			return nil, fmt.Errorf("parse json array: %w", err)
		}
		return normalizeProviderConfigs(list)
	}
	var cfg ProviderConfig
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	result, err := normalizeProviderConfigs([]ProviderConfig{cfg})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func normalizeProviderConfigs(configs []ProviderConfig) ([]ProviderConfig, error) {
	normalized := make([]ProviderConfig, 0, len(configs))
	for _, cfg := range configs {
		cfg.Name = strings.TrimSpace(cfg.Name)
		cfg.Type = strings.TrimSpace(cfg.Type)
		cfg.Model = strings.TrimSpace(cfg.Model)
		if cfg.Name == "" {
			return nil, fmt.Errorf("provider name is required")
		}
		if cfg.Type == "" {
			return nil, fmt.Errorf("provider %q type is required", cfg.Name)
		}
		if cfg.Model == "" {
			return nil, fmt.Errorf("provider %q model is required", cfg.Name)
		}
		resolvedAuth, err := resolveAuthPlaceholders(cfg.Auth)
		if err != nil {
			return nil, fmt.Errorf("provider %q auth error: %w", cfg.Name, err)
		}
		cfg.Auth = resolvedAuth
		normalized = append(normalized, cfg)
	}
	return normalized, nil
}

// SystemSecretResolverFunc decrypts a named secret from
// v1:platform:globalSecret. Wired by the engine before providers are
// loaded so resolveAuthPlaceholders can prefer concept storage over
// OS env. Returns ("", err) on miss; the caller's contract is to
// distinguish "not found" from "fatal" by examining the error.
type SystemSecretResolverFunc func(ctx context.Context, name string) (string, error)

// SystemVariableResolverFunc resolves a named plaintext variable from
// v1:platform:globalVariable. Same wiring + fallback pattern as
// SystemSecretResolverFunc.
type SystemVariableResolverFunc func(ctx context.Context, name string) (string, error)

var (
	systemSecretResolver   SystemSecretResolverFunc
	systemVariableResolver SystemVariableResolverFunc
)

// SetSystemSecretResolver installs a global resolver used by
// resolveAuthPlaceholders to prefer concept storage over the OS env
// when a provider's auth.apiKey is "${VAR_NAME}". The resolver is
// engine-bound; tests / standalone tools can leave it nil to use the
// pure env fallback.
func SetSystemSecretResolver(r SystemSecretResolverFunc) { systemSecretResolver = r }

// SetSystemVariableResolver is the plaintext counterpart for
// non-sensitive auth fields like baseURL or projectId. Same wiring
// rules as SetSystemSecretResolver.
func SetSystemVariableResolver(r SystemVariableResolverFunc) { systemVariableResolver = r }

// authConceptLookupNames returns the names to try in concept storage for a
// given placeholder name, in priority order.
//
// Providers reference `MEMQL_AI_<VENDOR>_...` (dsl/providers/providers.memql)
// because that is the OS env var the bridge-agent / STT bootstrap also reads,
// while operators seed the SEAL-FLOOR form -- `MEMQL_OPENAI_PROJECT_ID`,
// `MEMQL_ANTHROPIC_ORGANIZATION_ID` -- because that is the name the manifest and the
// docs give them. Rather than rename either side and force every install to
// re-seed, the resolver tries both:
//
//	MEMQL_AI_OPENAI_PROJECT_ID  (exact match for what the provider asked)
//	MEMQL_OPENAI_PROJECT_ID     (the seal-floor seeded form)
//
// First non-empty match wins, and the EXACT name is always first: seeding the
// precise name an operator was asked for must never be the losing option.
//
// # memql#4338
//
// This elided `MEMQL_SI_` and nothing else. Every provider in the tree asks
// for `MEMQL_AI_...`, and `MEMQL_SI_` survives only as a deprecated alias
// (component/envregistry/legacyalias.go) -- so the elision fired for NO
// provider and the fallback it exists to provide never happened. A key seeded
// under the documented `MEMQL_ANTHROPIC_ORGANIZATION_ID` was simply not found.
//
// The rename from `MEMQL_SI_` to `MEMQL_AI_` missed this constant, and three
// places kept describing the behaviour it had lost: this comment (which named
// `MEMQL_SI_` as the prefix and then gave a `MEMQL_AI_` example of it), the
// OS-env fallback comment below, and docs/public/operate/env-vars.md, which
// spells the mapping out literally. Documentation on three sides and code on
// none is what makes this the code's bug.
//
// It also elided the WHOLE prefix, yielding a bare `OPENAI_PROJECT_ID` rather
// than the `MEMQL_OPENAI_PROJECT_ID` every one of those three describes. Only the
// `AI_` / `SI_` segment is dropped now; `MEMQL_` is part of the seal-floor
// name.
//
// `MEMQL_SI_` is RETAINED rather than replaced, and its bare form is kept as a
// third candidate: a product DSL bundle mounted at MEMQL_DSL_PATH is read from
// disk at boot and may still declare the old prefix, so dropping either would
// break exactly the installs the alias table exists to carry.
//
// Names carrying neither prefix are looked up verbatim -- a synthesized
// fallback there would widen the search to a name nobody declared.
func authConceptLookupNames(envKey string) []string {
	for _, prefix := range []string{"MEMQL_AI_", "MEMQL_SI_"} {
		if !strings.HasPrefix(envKey, prefix) {
			continue
		}
		rest := strings.TrimPrefix(envKey, prefix)
		names := []string{envKey, "MEMQL_" + rest}
		if prefix == "MEMQL_SI_" {
			// Backwards compatibility only: what this function returned for
			// the old prefix before memql#4338. Last, so the documented name
			// wins whenever both are seeded.
			names = append(names, rest)
		}
		return names
	}
	return []string{envKey}
}

// authConceptResolver returns the value of a named entry in
// v1:platform:globalSecret first, then v1:platform:globalVariable. Encrypted /
// plaintext rows can both stand in for an auth field -- API keys are
// secrets, baseURL is a variable. Returns ("", false) if neither
// concept resolves the name (under any of the candidate names from
// authConceptLookupNames).
// CONCEPT STORAGE ONLY, still. It answers a narrower question than
// resolveAuthValueSourced -- "is this in the graph" rather than "can this be
// resolved at all" -- and it stays that way: widening it to include the env
// tier would silently change what its callers are asking, and the seal-floor
// naming test drives it precisely because it wants the concept path.
//
// Implemented over the shared chain rather than beside it (memql#4440) so
// there is exactly one walk of the candidate names, and the narrowing is a
// visible filter on the tier that answered.
func authConceptResolver(envKey string) (string, bool) {
	v, source, ok := resolveAuthValueSourced(envKey)
	if !ok || source == AuthSourceEnv {
		return "", false
	}
	return v, true
}

// ProviderAuthSource names WHICH tier of the resolution chain supplied a
// provider's credential (epic memql#4440, design D4).
//
// It exists so the portal's AI-providers page can tell an operator apart
// "the key is in the graph where I put it" from "the key is in this pod's
// environment and will vanish on the next image" -- two states that look
// identical in every previous surface, and which have completely different
// answers to "why did it stop working".
type ProviderAuthSource string

const (
	// AuthSourceFederation is Anthropic workload identity federation: no key
	// at rest anywhere, the pod's own projected token exchanged for a bearer.
	AuthSourceFederation ProviderAuthSource = "federation"
	// AuthSourceGlobalSecret is an encrypted v1:platform:globalSecret row --
	// what the portal page writes.
	AuthSourceGlobalSecret ProviderAuthSource = "globalSecret"
	// AuthSourceGlobalVariable is a plaintext v1:platform:globalVariable row.
	// Correct for a baseURL or a federation id, wrong for an API key.
	AuthSourceGlobalVariable ProviderAuthSource = "globalVariable"
	// AuthSourceEnv is the process environment -- the bootstrap-window
	// fallback. Legitimate, and worth showing as distinct: it is the one
	// source a portal write cannot change.
	AuthSourceEnv ProviderAuthSource = "env"
	// AuthSourceUnresolved is the keyless state, which after epic memql#4440
	// is the state a freshly installed cluster is in by design.
	AuthSourceUnresolved ProviderAuthSource = "unresolved"
)

// resolveAuthValueSourced is the ONE implementation of the resolution chain,
// reporting which tier answered.
//
// Extracted from resolveAuthPlaceholders rather than written beside it
// (memql#4440). The portal's status projection has to report the source, and
// a second walk of "globalSecret, then globalVariable, then env, with the
// prefix-elision candidate names" would be a second answer to the question
// this function exists to answer -- one that would drift the first time the
// chain changed and would then describe a resolution the engine did not make.
func resolveAuthValueSourced(envKey string) (string, ProviderAuthSource, bool) {
	ctx := context.Background()
	candidates := authConceptLookupNames(envKey)
	if systemSecretResolver != nil {
		for _, name := range candidates {
			if v, err := systemSecretResolver(ctx, name); err == nil {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed, AuthSourceGlobalSecret, true
				}
			}
		}
	}
	if systemVariableResolver != nil {
		for _, name := range candidates {
			if v, err := systemVariableResolver(ctx, name); err == nil {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed, AuthSourceGlobalVariable, true
				}
			}
		}
	}
	for _, name := range candidates {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, AuthSourceEnv, true
		}
	}
	return "", AuthSourceUnresolved, false
}

// resolveAuthPlaceholders walks a provider's auth map and substitutes
// any "${VAR_NAME}" placeholders with concrete values. Resolution
// order:
//
//  1. v1:platform:globalSecret    -- via systemSecretResolver, trying both
//     the prefixed and bare-name forms (see
//     authConceptLookupNames).
//  2. v1:platform:globalVariable  -- same, for non-sensitive auth fields
//     like baseURL.
//  3. OS env                -- bootstrap-window fallback. Providers
//     load eagerly during engine init, but
//     `make up-refresh` wipes the database
//     before re-seeding -- so on first boot
//     concept storage is empty when
//     providers load, and only the env keeps
//     them alive until the seed completes.
//     Retiring this requires either
//     lazy/per-request provider auth
//     resolution or a post-seed engine
//     reload; tracked as future work.
//
// If all three layers miss, the provider fails to load with a message
// telling the operator how to seed it.
func resolveAuthPlaceholders(values map[string]string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("auth configuration is required")
	}
	resolved := make(map[string]string, len(values))
	for key, value := range values {
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "${") && strings.HasSuffix(trimmed, "}") {
			envKey := strings.TrimSpace(trimmed[2 : len(trimmed)-1])
			if envKey == "" {
				return nil, fmt.Errorf("empty environment variable placeholder for auth %q", key)
			}
			// All three tiers, in one place: globalSecret, then
			// globalVariable, then OS env -- the last with the same
			// prefix-elision as concept storage, so `MEMQL_OPENAI_PROJECT_ID` in
			// env wins for a `MEMQL_AI_OPENAI_PROJECT_ID`-referencing provider,
			// matching the dev-manifest naming. The tier that answered is
			// discarded here and kept by the status projection
			// (providerAuthStatus), which is the only caller that needs it.
			candidates := authConceptLookupNames(envKey)
			if v, _, ok := resolveAuthValueSourced(envKey); ok {
				resolved[key] = v
				continue
			}
			// An OPTIONAL placeholder resolves to absent rather than to an
			// error (memql#4334). The whole set is Anthropic's credential --
			// see optionalAuthEnvNames for why absence is a legitimate
			// configuration there and a mistake everywhere else. The key is
			// simply left out of the resolved map, so a consumer reading it
			// gets "" and decides for itself.
			if optionalAuthPlaceholder(envKey) {
				continue
			}
			return nil, fmt.Errorf(
				// NAMES REAL THINGS ONLY (memql#4338). This used to direct
				// the operator at `secret-set` / `variable-set` make
				// targets; the Makefile has neither, and never has -- so
				// the one line an operator reads at the moment of failure
				// sent them to a command that does not exist.
				"auth %q references %s but no value is in concept storage or OS env. "+
					"Tried name(s) %s under v1:platform:globalSecret, v1:platform:globalVariable, "+
					"and the process env. Seed ANY of those names: put it in the node's "+
					"environment (locally `make secrets`; in a cluster, whichever secret store "+
					"the deployment reads), or store a v1:platform:globalSecret row under it. "+
					"The last name listed is the seal-floor form the manifest and "+
					"docs/public/operate/env-vars.md use",
				key, envKey, strings.Join(candidates, ", "))
		}
		if trimmed == "" {
			return nil, fmt.Errorf("auth value %q is empty", key)
		}
		resolved[key] = trimmed
	}
	return resolved, nil
}

// streamingHTTPClient returns an HTTP client tuned for AI streaming
// providers. We deliberately do NOT set http.Client.Timeout because
// streams are intentionally long-running; that field aborts the entire
// request including the body read. Instead we set transport-level
// timeouts: ResponseHeaderTimeout bounds the time-to-first-byte (the
// SDK fails fast if the upstream never starts responding) and
// IdleConnTimeout reaps idle pooled connections so the pool can
// recover from transient backend issues without us hanging on a stale
// keep-alive.
//
// Stream-progress (no chunks for N seconds while the connection is
// alive at the TCP level) is enforced one layer up, in the agent's
// consumeStreamingTurn idle watchdog. That layer is where we know
// what counts as "no progress" -- here we only know about TCP/HTTP,
// not SSE event semantics.
func streamingHTTPClient() *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = 30 * time.Second
	base.IdleConnTimeout = 90 * time.Second
	// Front the timeout-tuned transport with the global LLM circuit
	// breaker (memql#825) so streaming providers' calls are loop-guarded.
	return guardedHTTPClient(&http.Client{Transport: base})
}

func newAIProvider(cfg ProviderConfig) (AIProvider, error) {
	switch strings.ToLower(cfg.Type) {
	case "openai", "openaichat":
		if cfg.Streaming() {
			return newOpenAIStreamProvider(cfg)
		}
		return newOpenAIProvider(cfg)
	case "openaitts":
		return newOpenAITTSProvider(cfg)
	case "openaistt":
		return newOpenAISTTProvider(cfg)
	case "openaiembedding":
		return newOpenAIEmbeddingProvider(cfg)
	case "anthropic", "anthropicchat":
		if cfg.Streaming() {
			return newAnthropicStreamProvider(cfg)
		}
		return newAnthropicProvider(cfg)
	case "fleet":
		// The base `fleet` provider is @base and never reaches this switch.
		// Anything that DOES reach it is a static per-model child, which the
		// design does not have (epic memql#4676): a fleet model exists while a
		// laptop is awake, so a registry entry written at load would be a claim
		// nothing can keep. Refused with the alternative named, because
		// "unsupported provider type Fleet" would send the author looking for a
		// missing Go client rather than for the policy syntax that works.
		return nil, fmt.Errorf(
			"the fleet provider has no static per-model children; name the model from a "+
				"policy instead, as @primary(%q)", FleetReferencePrefix+cfg.Model)
	case "subscriptionapp":
		// Same reasoning, same refusal (epic memql#5096). An app door exists
		// while somebody is signed in on a machine that is awake, so a static
		// child would be a claim this tree cannot keep; the doors are
		// resolved from the live registrations at selection time.
		return nil, fmt.Errorf(
			"the app provider has no static per-app children; name the app from a "+
				"policy instead, as @fallback(%q)", AppReferencePrefix+cfg.Model)
	default:
		return nil, fmt.Errorf("unsupported provider type %q", cfg.Type)
	}
}

// ============================================================================
// OpenAI Embedding Provider
// ============================================================================

func newOpenAIEmbeddingProvider(cfg ProviderConfig) (AIProvider, error) {
	bearer, err := openAIBearerFor(cfg)
	if err != nil {
		return nil, err
	}
	// THE CALLER'S DEFAULT, out of range (memql#4779), and this is the one
	// site in the sweep where saturation would be actively worse: `dims` is
	// sent to OpenAI as the embedding vector width, so MaxInt and 0 are both
	// just a different 400 from the API. The default is already declared on
	// the line above and is the only answer that produces a working provider.
	dims := 1536
	if d, ok := cfg.Params["dimensions"]; ok {
		switch v := d.(type) {
		case float64:
			dims = num.Float64Or(v, dims)
		case int:
			dims = v
		}
	}
	client := NewOpenAIEmbeddingClient(cfg.Model, dims)
	client.bearer = bearer
	if baseURL := strings.TrimSpace(cfg.Auth["baseURL"]); baseURL != "" {
		client.baseURL = baseURL
	}
	return client, nil
}

// openAIBearerFor resolves the federated bearer source for one provider
// config, or the error the credential switch would have produced.
//
// It exists so the constructors that do NOT build an SDK client -- the
// embedding client, which reads its own response body -- take exactly the same
// credential decision as the ones that do. Two credential decisions for one
// vendor is how a cluster ends up with chat working and embeddings 401ing.
func openAIBearerFor(cfg ProviderConfig) (BearerSource, error) {
	_, source, path, err := openaiCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", cfg.Name, err)
	}
	if path == credentialPathUnavailable {
		return nil, fmt.Errorf(
			"provider %q has no OpenAI credential: workload identity federation is not configured on this cluster "+
				"(docs/public/operate/auth/openai-federation.md)", cfg.Name)
	}
	return source, nil
}

// ============================================================================
// OpenAI Chat Provider (Standard non-streaming)
// ============================================================================

// resolveOpenAIProjectId returns the OpenAI-Project header value for a
// provider config, preferring auth.projectId when the caller set it in
// a .memql override, and falling back to MEMQL_AI_OPENAI_PROJECT_ID
// from the environment. Returns "" when neither is set -- which is the
// steady state for service-account keys (sk-svcacct-*), which do not
// carry a project id. The openai-go client handles the empty case
// transparently (the header is simply not attached).
func resolveOpenAIProjectId(cfg ProviderConfig) string {
	if pid := strings.TrimSpace(cfg.Auth["projectId"]); pid != "" {
		return pid
	}
	return strings.TrimSpace(os.Getenv("MEMQL_AI_OPENAI_PROJECT_ID"))
}

// newOpenAIClient builds the official SDK's client for a provider config.
//
// ONE constructor for every OpenAI client the engine builds, for the reason
// newAnthropicClient exists: the credential is chosen in exactly one place. It
// takes a static key today; the OpenAI federation switch replaces that single
// option with the exchanger's middleware and nothing else in this file moves.
//
// The project id is an option rather than an HTTP wrapper now. Two hand-written
// wrappers used to set the OpenAI-Project header by composing over the client,
// which meant the header's presence depended on the ORDER the guarded transport
// and the wrapper were composed in -- and one of the two orders silently sent
// the header on some calls and not others.
func newOpenAIClient(cfg ProviderConfig, httpClient *http.Client) (*openai.Client, error) {
	client, _, err := newOpenAIClientWithCredential(cfg, httpClient)
	return client, err
}

// newOpenAIClientWithCredential is newOpenAIClient plus the bearer source, for
// the consumers that need the raw token rather than a client: the Realtime
// transcription WebSocket, which dials with an Authorization header of its own,
// and `provider-auth check`, which forces one exchange and reports it.
func newOpenAIClientWithCredential(cfg ProviderConfig, httpClient *http.Client) (*openai.Client, BearerSource, error) {
	opts, source, path, err := openaiCredential(cfg, httpClient)
	if err != nil {
		return nil, nil, err
	}
	if path == credentialPathUnavailable {
		// No credential is configured. This is the normal state of a fresh
		// cloud cluster and of every local one (design D2/D6), so it is
		// reported as an unavailable provider rather than as a fault -- the
		// registry's own wording, which the readiness model reads as `ai`
		// unconfigured.
		return nil, nil, fmt.Errorf(
			"provider %q has no OpenAI credential: workload identity federation is not configured on this cluster. "+
				"Set %s, %s and %s (docs/public/operate/auth/openai-federation.md). There is no API key to fall "+
				"back to -- a local cluster cannot federate, because its OIDC issuer is private, and reaches "+
				"models through a signed-in fleet machine or a local model instead",
			cfg.Name, envOpenAIIdentityProviderID, envOpenAIServiceAccountID, envOpenAIIdentityTokenFile)
	}
	if baseURL := strings.TrimSpace(cfg.Auth["baseURL"]); baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if projectId := resolveOpenAIProjectId(cfg); projectId != "" {
		opts = append(opts, option.WithProject(projectId))
	}
	client := openai.NewClient(opts...)
	return &client, source, nil
}

type openAIProvider struct {
	client *openai.Client
	model  string
	params map[string]any
}

// Compile-time interface assertions
var _ AIProvider = (*openAIProvider)(nil)
var _ common.ChatAIProvider = (*openAIProvider)(nil)
var _ common.ToolCallingChatAIProvider = (*openAIProvider)(nil)
var _ common.ChatStructuredProvider = (*openAIProvider)(nil)
var _ common.VisionAIProvider = (*openAIProvider)(nil)

func newOpenAIProvider(cfg ProviderConfig) (AIProvider, error) {
	// Route through the global LLM circuit breaker (memql#825).
	client, err := newOpenAIClient(cfg, guardedHTTPClient(nil))
	if err != nil {
		return nil, err
	}
	return &openAIProvider{
		client: client,
		model:  cfg.Model,
		params: cfg.Params,
	}, nil
}

func (p *openAIProvider) Call(ctx context.Context, prompt string) (any, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai provider is not configured")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}

	// Support both parameter names for compatibility
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned")
	}
	text := strings.TrimSpace(resp.Choices[0].Message.Content)
	if text == "" {
		return "", nil
	}

	var structured any
	if err := json.Unmarshal([]byte(text), &structured); err == nil {
		return structured, nil
	}
	return text, nil
}

// CallChat implements ChatAIProvider for proper multi-turn conversations.
// This sends messages with correct roles (system/user/assistant) to OpenAI,
// which is critical for the model to understand conversation context.
func (p *openAIProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("openai provider is not configured")
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: openAIMessages,
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// CallChatStructured implements provider-enforced structured output via
// OpenAI's json_schema response_format. When schema.Strict is true,
// OpenAI's constrained-decoding path guarantees the returned string
// parses against the schema -- no markdown fences, no missing fields,
// no enum leaks.
func (p *openAIProvider) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	content, _, err := p.CallChatStructuredWithUsage(ctx, messages, schema)
	return content, err
}

// CallChatStructuredWithUsage is the same call, reporting what it cost
// (epic memql#4661). The plain method above delegates to it rather than the
// other way round, so there is ONE request builder and one error taxonomy --
// two copies of this function is two places for the finish_reason handling
// below to drift.
func (p *openAIProvider) CallChatStructuredWithUsage(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, common.ChatUsage, error) {
	if p == nil || p.client == nil {
		return "", common.ChatUsage{}, fmt.Errorf("openai provider is not configured")
	}
	if len(messages) == 0 {
		return "", common.ChatUsage{}, fmt.Errorf("at least one message is required")
	}
	if len(schema.Schema) == 0 {
		return "", common.ChatUsage{}, fmt.Errorf("structured chat requires a non-empty schema")
	}
	name := strings.TrimSpace(schema.Name)
	if name == "" {
		name = "response"
	}

	// The official SDK types response_format's `schema` as `any` where the
	// community one took json.RawMessage. Decoding it here rather than handing
	// over the raw bytes is what makes the wire identical: a json.RawMessage
	// assigned to an `any` field marshals as a base64 STRING, not as the
	// object OpenAI expects, and the failure is a 400 about the schema rather
	// than about the encoding. The recorded fixture is what proves this.
	var schemaValue any
	if err := json.Unmarshal(schema.Schema, &schemaValue); err != nil {
		return "", common.ChatUsage{}, fmt.Errorf("structured chat schema is not valid JSON: %w", err)
	}

	openAIMessages := toOpenAIChatMessages(messages)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: openAIMessages,
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:        name,
					Description: openai.String(schema.Description),
					Schema:      schemaValue,
					Strict:      openai.Bool(schema.Strict),
				},
			},
		},
	}

	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return "", common.ChatUsage{}, err
	}
	// Reported=true even when both counts are zero: the API SAID something,
	// and "the provider reported zero" is a different fact from "the provider
	// said nothing", which is the whole distinction this type exists for.
	usage := common.ChatUsage{
		InputTokens:  int64(resp.Usage.PromptTokens),
		OutputTokens: int64(resp.Usage.CompletionTokens),
		Model:        resp.Model,
		Reported:     true,
	}
	if len(resp.Choices) == 0 {
		return "", common.ChatUsage{}, fmt.Errorf("no choices returned")
	}
	choice := resp.Choices[0]
	content := strings.TrimSpace(choice.Message.Content)
	if content == "" {
		// Empty content + no error from the SDK usually means one of:
		//   - the model produced a refusal (Refusal field set; the
		//     structured-output schema couldn't be satisfied or
		//     content moderation kicked in)
		//   - the completion-token cap was hit before any content
		//     was emitted (FinishReason == "length"; common with
		//     reasoning-capable models that consume the budget on
		//     internal thoughts)
		//   - the content filter stripped the entire reply
		//     (FinishReason == "content_filter")
		// Returning "" + nil here used to silently bubble through
		// the caller's JSON unmarshal as "unexpected end of JSON
		// input", which masked the actual cause. Surface it.
		if refusal := strings.TrimSpace(choice.Message.Refusal); refusal != "" {
			return "", common.ChatUsage{}, fmt.Errorf("model refused structured output: %s", refusal)
		}
		switch choice.FinishReason {
		case "length":
			return "", common.ChatUsage{}, fmt.Errorf(
				"empty content with finish_reason=length (model %q hit completion-token cap before producing output -- raise maxCompletionTokens or shrink the schema/input)",
				p.model,
			)
		case "content_filter":
			return "", common.ChatUsage{}, fmt.Errorf("empty content with finish_reason=content_filter (response was suppressed by safety filter)")
		case "":
			return "", common.ChatUsage{}, fmt.Errorf("empty content with no finish_reason returned (model %q may not exist or returned a malformed response)", p.model)
		default:
			return "", common.ChatUsage{}, fmt.Errorf("empty content with finish_reason=%s (model %q)", choice.FinishReason, p.model)
		}
	}
	return content, usage, nil
}

// CallChatWithTools implements tool-calling chat completion using OpenAI tools.
func (p *openAIProvider) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai provider is not configured")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)
	openAITools := toOpenAITools(tools)

	req := openai.ChatCompletionNewParams{
		Model:      openai.ChatModel(p.model),
		Messages:   openAIMessages,
		Tools:      openAITools,
		ToolChoice: openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")},
		// Prefer sequential tool calls for determinism/simplicity.
		ParallelToolCalls: openai.Bool(false),
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned")
	}

	msg := resp.Choices[0].Message
	result := &common.ToolCallingChatResult{
		AssistantText: strings.TrimSpace(msg.Content),
	}
	if len(msg.ToolCalls) > 0 {
		calls := make([]common.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			calls = append(calls, common.ToolCall{
				ID:        strings.TrimSpace(tc.ID),
				Name:      strings.TrimSpace(tc.Function.Name),
				Arguments: strings.TrimSpace(tc.Function.Arguments),
			})
		}
		result.ToolCalls = calls
	}
	return result, nil
}

// CallVision implements common.VisionAIProvider for the OpenAI provider.
func (p *openAIProvider) CallVision(ctx context.Context, prompt string, images []common.VisionContent) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("openai provider is not configured")
	}
	if len(images) == 0 {
		return "", fmt.Errorf("at least one image is required")
	}

	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(images)+1)
	for _, img := range images {
		encoded := base64.StdEncoding.EncodeToString(img.Data)
		dataURI := fmt.Sprintf("data:%s;base64,%s", img.MimeType, encoded)
		parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
			URL:    dataURI,
			Detail: "auto",
		}))
	}
	parts = append(parts, openai.TextContentPart(prompt))

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(parts)},
	}
	if maxTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	} else {
		req.MaxCompletionTokens = openai.Int(1000)
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return "", fmt.Errorf("vision api: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("vision api returned no choices")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// ============================================================================
// OpenAI Stream Provider (Streaming chat completion)
// ============================================================================

type openAIStreamProvider struct {
	client *openai.Client
	model  string
	params map[string]any
}

func newOpenAIStreamProvider(cfg ProviderConfig) (AIProvider, error) {
	// The streaming client is tuned for long-lived responses rather than the
	// guarded client's request/response shape.
	client, err := newOpenAIClient(cfg, streamingHTTPClient())
	if err != nil {
		return nil, err
	}
	return &openAIStreamProvider{
		client: client,
		model:  cfg.Model,
		params: cfg.Params,
	}, nil
}

// Call implements AIProvider with non-streaming fallback for compatibility.
func (p *openAIStreamProvider) Call(ctx context.Context, prompt string) (any, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai stream provider is not configured")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	// Use streaming but collect all chunks into final result
	chunks, err := p.CallStream(ctx, prompt)
	if err != nil {
		return nil, err
	}

	var result strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return nil, chunk.Error
		}
		result.WriteString(chunk.Content)
	}

	text := strings.TrimSpace(result.String())
	if text == "" {
		return "", nil
	}

	var structured any
	if err := json.Unmarshal([]byte(text), &structured); err == nil {
		return structured, nil
	}
	return text, nil
}

// CallChat implements ChatAIProvider for proper multi-turn conversations.
func (p *openAIStreamProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("openai stream provider is not configured")
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: openAIMessages,
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
	}
	return strings.TrimSpace(resp.Choices[0].Message.Content), nil
}

// CallChatWithTools implements tool-calling chat completion using OpenAI tools.
func (p *openAIStreamProvider) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai stream provider is not configured")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)
	openAITools := toOpenAITools(tools)

	req := openai.ChatCompletionNewParams{
		Model:             openai.ChatModel(p.model),
		Messages:          openAIMessages,
		Tools:             openAITools,
		ToolChoice:        openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")},
		ParallelToolCalls: openai.Bool(false),
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no choices returned")
	}

	msg := resp.Choices[0].Message
	result := &common.ToolCallingChatResult{
		AssistantText: strings.TrimSpace(msg.Content),
	}
	if len(msg.ToolCalls) > 0 {
		calls := make([]common.ToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			calls = append(calls, common.ToolCall{
				ID:        strings.TrimSpace(tc.ID),
				Name:      strings.TrimSpace(tc.Function.Name),
				Arguments: strings.TrimSpace(tc.Function.Arguments),
			})
		}
		result.ToolCalls = calls
	}
	return result, nil
}

func toOpenAITools(tools []common.ToolDefinition) []openai.ChatCompletionToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		params := t.InputSchema
		if params == nil {
			params = map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			}
		}
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        name,
			Description: openai.String(strings.TrimSpace(t.Description)),
			Parameters:  toFunctionParameters(params),
		}))
	}
	return out
}

// toFunctionParameters narrows a tool's declared input schema to the map the
// SDK's function-definition parameter takes.
//
// A tool whose schema is not an object is given the empty object schema rather
// than nil: `"parameters": null` is refused by OpenAI, and the refusal names
// the tool, not the schema that produced it.
func toFunctionParameters(schema any) shared.FunctionParameters {
	if m, ok := schema.(map[string]any); ok {
		return shared.FunctionParameters(m)
	}
	if m, ok := schema.(shared.FunctionParameters); ok {
		return m
	}
	return shared.FunctionParameters{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}

func toOpenAIChatMessages(messages []common.ChatMessage) []openai.ChatCompletionMessageParamUnion {
	if len(messages) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionMessageParamUnion, len(messages))
	for i, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system":
			out[i] = openai.SystemMessage(msg.Content)
		case "tool":
			out[i] = openai.ToolMessage(msg.Content, strings.TrimSpace(msg.ToolCallId))
		case "assistant":
			// The assistant message is built field by field rather than through
			// openai.AssistantMessage because it is the only role that carries
			// tool calls, and those must be preserved in the history or the
			// provider rejects the tool RESULT that follows them.
			assistant := &openai.ChatCompletionAssistantMessageParam{}
			if msg.Content != "" {
				assistant.Content.OfString = openai.String(msg.Content)
			}
			if name := strings.TrimSpace(msg.Name); name != "" {
				assistant.Name = openai.String(name)
			}
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(tc.Name) == "" {
					continue
				}
				assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: strings.TrimSpace(tc.ID),
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      strings.TrimSpace(tc.Name),
							Arguments: strings.TrimSpace(tc.Arguments),
						},
					},
				})
			}
			out[i] = openai.ChatCompletionMessageParamUnion{OfAssistant: assistant}
		default:
			// Everything unrecognised is a user turn, which is what the
			// community SDK's role default did.
			user := openai.UserMessage(msg.Content)
			if name := strings.TrimSpace(msg.Name); name != "" && user.OfUser != nil {
				user.OfUser.Name = openai.String(name)
			}
			out[i] = user
		}
	}
	return out
}

// CallStream implements StreamingAIProvider for streaming responses.
func (p *openAIStreamProvider) CallStream(ctx context.Context, prompt string) (<-chan StreamChunk, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai stream provider is not configured")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	// NewStreaming returns no error: the request failure surfaces on the first
	// Next()/Err() instead. That is why the error arm below is inside the
	// goroutine rather than beside the call -- a failed stream is delivered as
	// a chunk carrying the error, exactly as a mid-stream failure was.
	stream := p.client.Chat.Completions.NewStreaming(ctx, req)

	chunks := make(chan StreamChunk, 100)

	go func() {
		defer close(chunks)
		defer stream.Close()

		for stream.Next() {
			chunk := stream.Current()
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				chunks <- StreamChunk{Content: chunk.Choices[0].Delta.Content}
			}
		}
		if err := stream.Err(); err != nil {
			chunks <- StreamChunk{Error: err, Done: true}
			return
		}
		chunks <- StreamChunk{Done: true}
	}()

	return chunks, nil
}

// CallChatStream streams a multi-turn chat completion (no tools).
// Implements common.ChatStreamProvider.
func (p *openAIStreamProvider) CallChatStream(ctx context.Context, messages []common.ChatMessage) (<-chan common.StreamChunk, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai stream provider is not configured")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: openAIMessages,
	}

	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, req)

	chunks := make(chan common.StreamChunk, 100)

	go func() {
		defer close(chunks)
		defer stream.Close()

		for stream.Next() {
			chunk := stream.Current()
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				chunks <- common.StreamChunk{Content: chunk.Choices[0].Delta.Content}
			}
		}
		if err := stream.Err(); err != nil {
			chunks <- common.StreamChunk{Error: err, Done: true}
			return
		}
		chunks <- common.StreamChunk{Done: true}
	}()

	return chunks, nil
}

// CallChatStreamWithTools streams a multi-turn chat completion with tool support.
// Both text content deltas and tool call deltas arrive on the same channel.
func (p *openAIStreamProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai stream provider is not configured")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)
	openAITools := toOpenAITools(tools)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(p.model),
		Messages: openAIMessages,
	}
	if len(openAITools) > 0 {
		req.Tools = openAITools
		req.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{OfAuto: openai.String("auto")}
		// Let the model emit multiple sequential tool calls in one
		// response. Critical for multi-step takeovers (e.g. a frontend
		// UI-operator session = request + navigate + click + click +
		// release) where forcing one-per-response burns through the
		// streaming tool-loop iteration budget and the agent stalls
		// mid-sequence. The server-side loop executes parallel calls
		// in order and feeds all results back before the next turn.
		req.ParallelToolCalls = openai.Bool(true)
	}

	if _, ok := p.params["temperature"]; ok {
		req.Temperature = openai.Float(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = openai.Float(numberParam(p.params["topP"], 1.0))
	}
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxCompletionTokens))
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = openai.Int(int64(maxTokens))
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, req)

	chunks := make(chan common.StreamToolChunk, 100)

	go func() {
		defer close(chunks)
		defer stream.Close()

		for stream.Next() {
			current := stream.Current()
			if len(current.Choices) == 0 {
				continue
			}

			delta := current.Choices[0].Delta
			chunk := common.StreamToolChunk{Content: delta.Content}

			for _, tc := range delta.ToolCalls {
				// Index is a plain int64 in the official SDK where the
				// community one made it a *int. The zero value means the same
				// thing in both -- the first tool call of the turn -- so the
				// nil-check the pointer needed simply goes away.
				chunk.ToolCalls = append(chunk.ToolCalls, common.ToolCallDelta{
					Index:     int(tc.Index),
					ID:        strings.TrimSpace(tc.ID),
					Name:      strings.TrimSpace(tc.Function.Name),
					Arguments: tc.Function.Arguments,
				})
			}

			if chunk.Content != "" || len(chunk.ToolCalls) > 0 {
				chunks <- chunk
			}
		}
		if err := stream.Err(); err != nil {
			chunks <- common.StreamToolChunk{Error: err, Done: true}
			return
		}
		chunks <- common.StreamToolChunk{Done: true}
	}()

	return chunks, nil
}

// Ensure openAIStreamProvider implements StreamingAIProvider, ChatStreamProvider, and ChatStreamWithToolsProvider
var _ StreamingAIProvider = (*openAIStreamProvider)(nil)
var _ common.ChatStreamProvider = (*openAIStreamProvider)(nil)
var _ common.ChatStreamWithToolsProvider = (*openAIStreamProvider)(nil)

// ============================================================================
// Anthropic Chat Provider (Standard non-streaming)
// ============================================================================

type anthropicProvider struct {
	client anthropic.Client
	model  string
	params map[string]any
}

// Compile-time interface assertions
var _ AIProvider = (*anthropicProvider)(nil)
var _ common.ChatAIProvider = (*anthropicProvider)(nil)
var _ common.ToolCallingChatAIProvider = (*anthropicProvider)(nil)
var _ common.VisionAIProvider = (*anthropicProvider)(nil)

func newAnthropicProvider(cfg ProviderConfig) (AIProvider, error) {
	// Which credential this client uses -- a static key or a federated,
	// short-lived bearer -- is anthropicCredential's decision, not this
	// constructor's (memql#4334). The guarded http.Client (the global LLM
	// circuit breaker, memql#825) is attached in every branch.
	client, _, err := newAnthropicClient(cfg, guardedHTTPClient(nil))
	if err != nil {
		return nil, err
	}
	return &anthropicProvider{
		client: client,
		model:  cfg.Model,
		params: cfg.Params,
	}, nil
}

func (p *anthropicProvider) Call(ctx context.Context, prompt string) (any, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	maxTokens := int64(4096)
	if mt, ok := intParam(p.params["maxTokens"]); ok {
		maxTokens = int64(mt)
	} else if mt, ok := intParam(p.params["maxCompletionTokens"]); ok {
		maxTokens = int64(mt)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: maxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	}

	if _, ok := p.params["temperature"]; ok {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}
	if _, ok := p.params["topP"]; ok {
		params.TopP = anthropic.Float(numberParam(p.params["topP"], 1.0))
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, err
	}

	text := extractAnthropicText(resp)
	if text == "" {
		return "", nil
	}

	var structured any
	if err := json.Unmarshal([]byte(text), &structured); err == nil {
		return structured, nil
	}
	return text, nil
}

func (p *anthropicProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("at least one message is required")
	}

	anthropicMessages, systemBlocks := toAnthropicMessages(messages)

	maxTokens := int64(4096)
	if mt, ok := intParam(p.params["maxTokens"]); ok {
		maxTokens = int64(mt)
	} else if mt, ok := intParam(p.params["maxCompletionTokens"]); ok {
		maxTokens = int64(mt)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: maxTokens,
		Messages:  anthropicMessages,
	}

	if len(systemBlocks) > 0 {
		params.System = systemBlocks
	}
	if _, ok := p.params["temperature"]; ok {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}
	if _, ok := p.params["topP"]; ok {
		params.TopP = anthropic.Float(numberParam(p.params["topP"], 1.0))
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return "", err
	}

	return extractAnthropicText(resp), nil
}

func (p *anthropicProvider) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}

	anthropicMessages, systemBlocks := toAnthropicMessages(messages)
	anthropicTools := toAnthropicTools(tools)

	maxTokens := int64(4096)
	if mt, ok := intParam(p.params["maxTokens"]); ok {
		maxTokens = int64(mt)
	} else if mt, ok := intParam(p.params["maxCompletionTokens"]); ok {
		maxTokens = int64(mt)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: maxTokens,
		Messages:  anthropicMessages,
	}

	if len(systemBlocks) > 0 {
		// §M: when this provider is configured with
		// params.enablePromptCache=true, mark the last system block
		// with cache_control=ephemeral so Anthropic caches everything
		// up to and including it. The System scope fence in
		// agentReply.tmpl is the biggest static portion; turning this
		// on cuts input cost ~90% + first-token latency on cache hits
		// and is safe on misses (no behaviour change, just no cache
		// benefit). Ops control per-provider via the registry.
		if boolParam(p.params["enablePromptCache"]) {
			last := len(systemBlocks) - 1
			systemBlocks[last].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		params.System = systemBlocks
	}
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAuto: &anthropic.ToolChoiceAutoParam{
				DisableParallelToolUse: anthropic.Bool(true),
			},
		}
	}
	if _, ok := p.params["temperature"]; ok {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}
	if _, ok := p.params["topP"]; ok {
		params.TopP = anthropic.Float(numberParam(p.params["topP"], 1.0))
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, err
	}

	result := &common.ToolCallingChatResult{
		AssistantText: extractAnthropicText(resp),
	}

	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			result.ToolCalls = append(result.ToolCalls, common.ToolCall{
				ID:        strings.TrimSpace(block.ID),
				Name:      strings.TrimSpace(block.Name),
				Arguments: strings.TrimSpace(string(block.Input)),
			})
		}
	}

	return result, nil
}

// CallVision implements common.VisionAIProvider for the Anthropic provider.
func (p *anthropicProvider) CallVision(ctx context.Context, prompt string, images []common.VisionContent) (string, error) {
	if len(images) == 0 {
		return "", fmt.Errorf("at least one image is required")
	}

	maxTokens := int64(1000)
	if mt, ok := intParam(p.params["maxTokens"]); ok {
		maxTokens = int64(mt)
	} else if mt, ok := intParam(p.params["maxCompletionTokens"]); ok {
		maxTokens = int64(mt)
	}

	contentBlocks := make([]anthropic.ContentBlockParamUnion, 0, len(images)+1)
	for _, img := range images {
		encoded := base64.StdEncoding.EncodeToString(img.Data)
		contentBlocks = append(contentBlocks, anthropic.NewImageBlockBase64(img.MimeType, encoded))
	}
	contentBlocks = append(contentBlocks, anthropic.NewTextBlock(prompt))

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: maxTokens,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(contentBlocks...)},
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("vision api: %w", err)
	}
	return strings.TrimSpace(extractAnthropicText(resp)), nil
}

// ============================================================================
// Anthropic Stream Provider (Streaming chat completion)
// ============================================================================

type anthropicStreamProvider struct {
	client  anthropic.Client
	model   string
	name    string
	pricing Pricing
	params  map[string]any
}

// Compile-time interface assertions
var _ StreamingAIProvider = (*anthropicStreamProvider)(nil)
var _ common.ChatStreamProvider = (*anthropicStreamProvider)(nil)
var _ common.ChatStreamWithToolsProvider = (*anthropicStreamProvider)(nil)
var _ common.ChatAIProvider = (*anthropicStreamProvider)(nil)
var _ common.ToolCallingChatAIProvider = (*anthropicStreamProvider)(nil)

func newAnthropicStreamProvider(cfg ProviderConfig) (AIProvider, error) {
	// Same credential decision as the non-streaming constructor, over the
	// streaming-tuned (and still guarded) http.Client (memql#4334).
	client, _, err := newAnthropicClient(cfg, streamingHTTPClient())
	if err != nil {
		return nil, err
	}
	return &anthropicStreamProvider{
		client:  client,
		model:   cfg.Model,
		name:    cfg.Name,
		pricing: cfg.Pricing(),
		params:  cfg.Params,
	}, nil
}

func (p *anthropicStreamProvider) anthropicMaxTokens() int64 {
	maxTokens := int64(4096)
	if mt, ok := intParam(p.params["maxTokens"]); ok {
		maxTokens = int64(mt)
	} else if mt, ok := intParam(p.params["maxCompletionTokens"]); ok {
		maxTokens = int64(mt)
	}
	return maxTokens
}

// anthropicThinking returns the extended-thinking config to attach to
// MessageNewParams.Thinking. Opt-in via the provider's params block:
//
//	params {
//	  thinkingBudgetTokens 8192   // >= 1024, must be < maxTokens
//	  // ...
//	}
//
// When the budget is unset / < 1024, returns the zero union (which
// the SDK omits from the request). When set, returns
// ThinkingConfigParamOfEnabled(budget) -- the SDK's helper that
// constructs the {type: "enabled", budget_tokens: N} shape Anthropic's
// extended thinking surface expects.
//
// The two also-supported config types (Adaptive, Disabled) are not
// wired here -- the user-facing knob is "is thinking on, and how
// much can it spend?" Adding Adaptive is a follow-up if the
// `adaptive` heuristic turns out to be worth surfacing per-agent.
//
// API constraint: when thinking is enabled, Anthropic requires
// temperature == 1 (or omitted). Callers that detect a non-omitted
// thinking union must therefore SKIP applying p.params["temperature"]
// or the request fails with a 400. anthropicThinkingEnabled() below
// is the predicate every params-building call site reads.
func (p *anthropicStreamProvider) anthropicThinking() anthropic.ThinkingConfigParamUnion {
	budget, ok := intParam(p.params["thinkingBudgetTokens"])
	if !ok {
		return anthropic.ThinkingConfigParamUnion{}
	}
	if budget < 1024 {
		// Below the API floor. Treat as off rather than erroring at
		// request time -- the provider definition's intent was clearly
		// "no thinking" even if the author miscalibrated the budget.
		return anthropic.ThinkingConfigParamUnion{}
	}
	return anthropic.ThinkingConfigParamOfEnabled(int64(budget))
}

// anthropicThinkingEnabled mirrors anthropicThinking()'s decision
// without allocating the union. Used by the temperature-suppression
// branch in every call site: thinking + non-1 temperature == 400 from
// the API, so we silently skip applying the temperature param when
// thinking is on.
func (p *anthropicStreamProvider) anthropicThinkingEnabled() bool {
	budget, ok := intParam(p.params["thinkingBudgetTokens"])
	return ok && budget >= 1024
}

func (p *anthropicStreamProvider) Call(ctx context.Context, prompt string) (any, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	chunks, err := p.CallStream(ctx, prompt)
	if err != nil {
		return nil, err
	}
	var result strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			return nil, chunk.Error
		}
		result.WriteString(chunk.Content)
	}
	text := strings.TrimSpace(result.String())
	if text == "" {
		return "", nil
	}
	var structured any
	if err := json.Unmarshal([]byte(text), &structured); err == nil {
		return structured, nil
	}
	return text, nil
}

// CallChat implements ChatAIProvider (non-streaming fallback).
func (p *anthropicStreamProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	if len(messages) == 0 {
		return "", fmt.Errorf("at least one message is required")
	}
	anthropicMessages, systemBlocks := toAnthropicMessages(messages)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.anthropicMaxTokens(),
		Messages:  anthropicMessages,
		Thinking:  p.anthropicThinking(),
	}
	if len(systemBlocks) > 0 {
		params.System = systemBlocks
	}
	if _, ok := p.params["temperature"]; ok && !p.anthropicThinkingEnabled() {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return "", err
	}
	return extractAnthropicText(resp), nil
}

// CallChatWithTools implements ToolCallingChatAIProvider (non-streaming fallback).
func (p *anthropicStreamProvider) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}
	anthropicMessages, systemBlocks := toAnthropicMessages(messages)
	anthropicTools := toAnthropicTools(tools)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.anthropicMaxTokens(),
		Messages:  anthropicMessages,
		Thinking:  p.anthropicThinking(),
	}
	if len(systemBlocks) > 0 {
		params.System = systemBlocks
	}
	if len(anthropicTools) > 0 {
		params.Tools = anthropicTools
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAuto: &anthropic.ToolChoiceAutoParam{
				DisableParallelToolUse: anthropic.Bool(true),
			},
		}
	}
	if _, ok := p.params["temperature"]; ok && !p.anthropicThinkingEnabled() {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, err
	}
	result := &common.ToolCallingChatResult{
		AssistantText: extractAnthropicText(resp),
	}
	for _, block := range resp.Content {
		if block.Type == "tool_use" {
			result.ToolCalls = append(result.ToolCalls, common.ToolCall{
				ID:        strings.TrimSpace(block.ID),
				Name:      strings.TrimSpace(block.Name),
				Arguments: strings.TrimSpace(string(block.Input)),
			})
		}
	}
	return result, nil
}

// CallStream implements StreamingAIProvider for single-prompt streaming.
func (p *anthropicStreamProvider) CallStream(ctx context.Context, prompt string) (<-chan StreamChunk, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.anthropicMaxTokens(),
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
		Thinking:  p.anthropicThinking(),
	}
	if _, ok := p.params["temperature"]; ok && !p.anthropicThinkingEnabled() {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}
	stream := p.client.Messages.NewStreaming(ctx, params)
	chunks := make(chan StreamChunk, 100)
	go func() {
		defer close(chunks)
		// Track usage so we can log a single per-call summary line
		// with token + cost numbers. Same pattern as
		// CallChatStreamWithTools; surfacing the same data on the
		// simpler InvokeAI path so callers like the planner (which
		// goes through Call -> CallStream, not the tools variant)
		// also produce a grep-able cost line per LLM round trip.
		var (
			usageInput         int64
			usageCacheCreation int64
			usageCacheRead     int64
			usageOutput        int64
		)
		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "message_start":
				u := event.Message.Usage
				usageInput = u.InputTokens
				usageCacheCreation = u.CacheCreationInputTokens
				usageCacheRead = u.CacheReadInputTokens
			case "message_delta":
				u := event.Usage
				usageOutput = u.OutputTokens
				if u.CacheCreationInputTokens > 0 {
					usageCacheCreation = u.CacheCreationInputTokens
				}
				if u.CacheReadInputTokens > 0 {
					usageCacheRead = u.CacheReadInputTokens
				}
			case "content_block_delta":
				if event.Delta.Type == "text_delta" {
					chunks <- StreamChunk{Content: event.Delta.Text}
				}
			}
		}
		// Log usage + cost. Computed cost relies on the provider's
		// declared Pricing (cfg.Pricing()); when pricing isn't
		// configured CostFor returns zeros, which is fine -- the
		// token counts alone tell the operator what was spent.
		_, _, _, totalUSD := p.pricing.CostFor(
			int(usageInput+usageCacheCreation),
			int(usageOutput),
			int(usageCacheRead),
		)
		slog.Info("anthropic stream: usage",
			"provider", p.name,
			"model", p.model,
			"input_tokens", usageInput,
			"output_tokens", usageOutput,
			"cache_creation_tokens", usageCacheCreation,
			"cache_read_tokens", usageCacheRead,
			"cost_usd", totalUSD,
		)
		if err := stream.Err(); err != nil {
			chunks <- StreamChunk{Error: err, Done: true}
			return
		}
		chunks <- StreamChunk{Done: true}
	}()
	return chunks, nil
}

// CallChatStream implements ChatStreamProvider.
func (p *anthropicStreamProvider) CallChatStream(ctx context.Context, messages []common.ChatMessage) (<-chan common.StreamChunk, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}
	anthropicMessages, systemBlocks := toAnthropicMessages(messages)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.anthropicMaxTokens(),
		Messages:  anthropicMessages,
		Thinking:  p.anthropicThinking(),
	}
	if len(systemBlocks) > 0 {
		applyAnthropicSystemPromptCache(systemBlocks, p.params)
		params.System = systemBlocks
	}
	if _, ok := p.params["temperature"]; ok && !p.anthropicThinkingEnabled() {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}
	stream := p.client.Messages.NewStreaming(ctx, params)
	chunks := make(chan common.StreamChunk, 100)
	go func() {
		defer close(chunks)
		for stream.Next() {
			event := stream.Current()
			if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" {
				chunks <- common.StreamChunk{Content: event.Delta.Text}
			}
		}
		if err := stream.Err(); err != nil {
			chunks <- common.StreamChunk{Error: err, Done: true}
			return
		}
		chunks <- common.StreamChunk{Done: true}
	}()
	return chunks, nil
}

// CallChatStreamWithTools implements ChatStreamWithToolsProvider.
func (p *anthropicStreamProvider) CallChatStreamWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (<-chan common.StreamToolChunk, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}
	anthropicMessages, systemBlocks := toAnthropicMessages(messages)
	anthropicTools := toAnthropicTools(tools)
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.anthropicMaxTokens(),
		Messages:  anthropicMessages,
		Thinking:  p.anthropicThinking(),
	}
	if len(systemBlocks) > 0 {
		applyAnthropicSystemPromptCache(systemBlocks, p.params)
		params.System = systemBlocks
	}
	if len(anthropicTools) > 0 {
		applyAnthropicToolPromptCache(anthropicTools, p.params)
		params.Tools = anthropicTools
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAuto: &anthropic.ToolChoiceAutoParam{
				DisableParallelToolUse: anthropic.Bool(true),
			},
		}
	}
	if _, ok := p.params["temperature"]; ok && !p.anthropicThinkingEnabled() {
		params.Temperature = anthropic.Float(numberParam(p.params["temperature"], 1.0))
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	// Peek the first event synchronously so HTTP 4xx errors from
	// Anthropic (auth, billing, rate-limit, malformed request) come
	// back as PRE-FLIGHT failures instead of mid-stream. Pre-flight
	// failures trigger the router's fallback chain
	// (component/router/fallback.go); mid-stream failures don't,
	// because mid-stream replay would need a buffer (deferred work).
	//
	// In normal operation the first event is `message_start` (carries
	// usage tokens). A failed call hits stream.Err() on the first
	// Next() and we surface it as `(nil, err)` so the router can
	// advance to the next provider in the chain.
	if !stream.Next() {
		if err := stream.Err(); err != nil {
			return nil, fmt.Errorf("anthropic stream pre-flight: %w", err)
		}
		// Stream finished without producing any event. Treat as a
		// no-op pre-flight failure so the fallback chain takes over.
		return nil, fmt.Errorf("anthropic stream pre-flight: empty stream")
	}
	firstEvent := stream.Current()

	chunks := make(chan common.StreamToolChunk, 100)

	go func() {
		defer close(chunks)

		// Track current tool call being streamed
		var currentToolIndex int
		var currentToolId string
		var currentToolName string

		// Track token + cache usage across the stream so we can log a
		// single summary line per call. message_start carries initial
		// usage (input + cache_read / cache_creation); message_delta
		// updates running totals as tokens come in. Logging these at
		// `done` is the cheapest way to confirm prompt caching is
		// actually hitting vs. every turn being a cold cache miss --
		// see agentReply.tmpl's APP KNOWLEDGE block and the
		// applyAnthropicSystemPromptCache / ...Tool paths for the
		// cache_control breakpoints we set.
		var (
			usageInput         int64
			usageCacheCreation int64
			usageCacheRead     int64
			usageOutput        int64
		)

		// Process the first event we peeked at outside the goroutine,
		// then continue with stream.Next() for the rest. Wrapping in a
		// closure keeps the per-event switch logic in one place; the
		// peeked event flows through the same handling as subsequent
		// events.
		processEvent := func(event anthropic.MessageStreamEventUnion) {
			switch event.Type {
			case "message_start":
				u := event.Message.Usage
				usageInput = u.InputTokens
				usageCacheCreation = u.CacheCreationInputTokens
				usageCacheRead = u.CacheReadInputTokens
			case "message_delta":
				u := event.Usage
				usageOutput = u.OutputTokens
				if u.CacheCreationInputTokens > 0 {
					usageCacheCreation = u.CacheCreationInputTokens
				}
				if u.CacheReadInputTokens > 0 {
					usageCacheRead = u.CacheReadInputTokens
				}
			case "content_block_start":
				if event.ContentBlock.Type == "tool_use" {
					currentToolId = event.ContentBlock.ID
					currentToolName = event.ContentBlock.Name
					currentToolIndex = int(event.Index)
					chunks <- common.StreamToolChunk{
						ToolCalls: []common.ToolCallDelta{{
							Index: currentToolIndex,
							ID:    currentToolId,
							Name:  currentToolName,
						}},
					}
				}
			case "content_block_delta":
				if event.Delta.Type == "text_delta" {
					chunks <- common.StreamToolChunk{Content: event.Delta.Text}
				} else if event.Delta.Type == "input_json_delta" {
					chunks <- common.StreamToolChunk{
						ToolCalls: []common.ToolCallDelta{{
							Index:     currentToolIndex,
							Arguments: event.Delta.PartialJSON,
						}},
					}
				}
			}
		}

		processEvent(firstEvent)
		for stream.Next() {
			processEvent(stream.Current())
		}

		// `hit_ratio` is read_tokens / (input + cache_creation + read)
		// so you can tell at a glance whether caching is doing work:
		// 0.0 on a cold turn, high (often >0.9) once the prefix is
		// warm. Creation tokens bill like input; read tokens bill at
		// the discounted cache tier. Numbers logged in token units so
		// the ops team can feed them directly into cost math.
		totalInput := usageInput + usageCacheCreation + usageCacheRead
		hitRatio := 0.0
		if totalInput > 0 {
			hitRatio = float64(usageCacheRead) / float64(totalInput)
		}
		slog.Info("anthropic stream: usage",
			"model", p.model,
			"input_tokens", usageInput,
			"output_tokens", usageOutput,
			"cache_creation_tokens", usageCacheCreation,
			"cache_read_tokens", usageCacheRead,
			"hit_ratio", hitRatio,
			"cache_enabled", boolParam(p.params["enablePromptCache"]),
		)

		if err := stream.Err(); err != nil {
			chunks <- common.StreamToolChunk{Error: err, Done: true}
			return
		}
		chunks <- common.StreamToolChunk{Done: true}
	}()

	return chunks, nil
}

// ============================================================================
// Anthropic Helper Functions
// ============================================================================

// toAnthropicMessages converts common.ChatMessage to Anthropic format.
// System messages are extracted separately since Anthropic uses a top-level system parameter.
func toAnthropicMessages(messages []common.ChatMessage) ([]anthropic.MessageParam, []anthropic.TextBlockParam) {
	var anthropicMessages []anthropic.MessageParam
	var systemBlocks []anthropic.TextBlockParam

	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))

		switch role {
		case "system":
			if strings.TrimSpace(msg.Content) != "" {
				systemBlocks = append(systemBlocks, anthropic.TextBlockParam{
					Text: msg.Content,
				})
			}

		case "user":
			var blocks []anthropic.ContentBlockParamUnion
			if strings.TrimSpace(msg.Content) != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			// If this is a tool result message (role=user with ToolCallId), use tool_result block
			if msg.ToolCallId != "" {
				blocks = []anthropic.ContentBlockParamUnion{
					anthropic.NewToolResultBlock(msg.ToolCallId, msg.Content, false),
				}
			}
			if len(blocks) > 0 {
				anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
					Role:    anthropic.MessageParamRoleUser,
					Content: blocks,
				})
			}

		case "tool":
			// Tool results in Anthropic are sent as user messages with tool_result blocks
			anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					anthropic.NewToolResultBlock(msg.ToolCallId, msg.Content, false),
				},
			})

		case "assistant":
			var blocks []anthropic.ContentBlockParamUnion
			if strings.TrimSpace(msg.Content) != "" {
				blocks = append(blocks, anthropic.NewTextBlock(msg.Content))
			}
			// Preserve tool calls from assistant messages
			for _, tc := range msg.ToolCalls {
				var inputRaw json.RawMessage
				if strings.TrimSpace(tc.Arguments) != "" {
					inputRaw = json.RawMessage(tc.Arguments)
				} else {
					inputRaw = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tc.ID,
						Name:  tc.Name,
						Input: inputRaw,
					},
				})
			}
			if len(blocks) > 0 {
				anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
					Role:    anthropic.MessageParamRoleAssistant,
					Content: blocks,
				})
			}
		}
	}

	// Anthropic REQUIRES at least one entry in messages -- system content is
	// a top-level parameter, not a turn -- so a system-only conversation
	// (the shape InvokeAIStructured's last-resort fallback builds) would
	// otherwise go out with an empty list and come back
	// 400 invalid_request_error "messages: Field required". OpenAI accepts a
	// system-only list, which is why every structured prompt worked until
	// the first Claude-only cluster ran one. Synthesize the missing user
	// turn instead of failing the call: the instructions are all in the
	// system blocks, so a minimal directive is the whole turn.
	if len(anthropicMessages) == 0 {
		anthropicMessages = append(anthropicMessages, anthropic.MessageParam{
			Role:    anthropic.MessageParamRoleUser,
			Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("Proceed as instructed.")},
		})
	}

	return anthropicMessages, systemBlocks
}

// toAnthropicTools converts common tool definitions to Anthropic format.
func toAnthropicTools(tools []common.ToolDefinition) []anthropic.ToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}

		// Convert InputSchema to ToolInputSchemaParam
		inputSchema := anthropic.ToolInputSchemaParam{
			Properties: map[string]any{},
		}

		if t.InputSchema != nil {
			if schemaMap, ok := t.InputSchema.(map[string]any); ok {
				if props, ok := schemaMap["properties"]; ok {
					inputSchema.Properties = props
				}
				if req, ok := schemaMap["required"].([]any); ok {
					for _, r := range req {
						if s, ok := r.(string); ok {
							inputSchema.Required = append(inputSchema.Required, s)
						}
					}
				}
				// Pass through extra fields like "additionalProperties"
				for k, v := range schemaMap {
					if k != "type" && k != "properties" && k != "required" {
						if inputSchema.ExtraFields == nil {
							inputSchema.ExtraFields = make(map[string]any)
						}
						inputSchema.ExtraFields[k] = v
					}
				}
			}
		}

		tool := anthropic.ToolParam{
			Name:        name,
			Description: param.Opt[string]{Value: strings.TrimSpace(t.Description)},
			InputSchema: inputSchema,
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: &tool})
	}
	return out
}

// applyAnthropicSystemPromptCache marks the LAST system block with
// cache_control=ephemeral when the provider config has
// enablePromptCache=true. Anthropic caches the entire prefix up to and
// including any block carrying that marker. For the agentReply.tmpl
// path the bulk of the system message is static across iterations
// (scope fence, tool playbook, RAG block which is built from the
// turn's first message and reused across iterations) so the cache
// hits cut input cost ~90% and meaningfully reduce first-token
// latency. Cache misses are identical to today (no behaviour change).
//
// Mirrors the gating used by anthropicProvider.CallChatWithTools (the
// non-stream variant landed in commit f9e4cf8); this is the streaming
// counterpart so operator turns -- which always go through the stream
// path -- get the same benefit.
func applyAnthropicSystemPromptCache(systemBlocks []anthropic.TextBlockParam, params map[string]any) {
	if len(systemBlocks) == 0 {
		return
	}
	if !boolParam(params["enablePromptCache"]) {
		return
	}
	last := len(systemBlocks) - 1
	systemBlocks[last].CacheControl = anthropic.NewCacheControlEphemeralParam()
}

// applyAnthropicToolPromptCache marks the LAST tool definition with
// cache_control=ephemeral when enablePromptCache is on. Anthropic
// places tool defs in the cacheable prefix BEFORE the system block,
// so a marker on the last tool extends the cached prefix to cover all
// tool schemas. For the operator agent (~17 tool defs, mostly static
// across iterations) this is a meaningful additional saving on top of
// the system-block marker.
//
// Anthropic allows up to 4 cache_control breakpoints per request; we
// use 2 (last tool + last system block). Adding more is possible
// later if we identify other static prefixes worth marking.
func applyAnthropicToolPromptCache(tools []anthropic.ToolUnionParam, params map[string]any) {
	if len(tools) == 0 {
		return
	}
	if !boolParam(params["enablePromptCache"]) {
		return
	}
	last := &tools[len(tools)-1]
	if last.OfTool != nil {
		last.OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
}

// extractAnthropicText extracts all text content from an Anthropic response.
func extractAnthropicText(resp *anthropic.Message) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, block := range resp.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	return strings.Join(parts, "\n")
}

// ============================================================================
// OpenAI TTS Provider (Text-to-Speech)
// ============================================================================

// Target duration per chunk for progressive decode (milliseconds)
// 200ms provides good balance between latency and overhead
const ttsTargetChunkDurationMS = 200

type openAITTSProvider struct {
	client *openai.Client
	model  string
	params map[string]any
}

func newOpenAITTSProvider(cfg ProviderConfig) (AIProvider, error) {
	// Speech rides the guarded transport for the first time here (memql#5088).
	// The guard fingerprints /chat/completions only, so this call is observed
	// by the transport and deliberately not counted against the LLM loop caps.
	client, err := newOpenAIClient(cfg, guardedHTTPClient(nil))
	if err != nil {
		return nil, err
	}
	return &openAITTSProvider{
		client: client,
		model:  cfg.Model,
		params: cfg.Params,
	}, nil
}

// speak performs one /audio/speech call and returns the raw audio bytes.
//
// Both callers -- Synthesize, which honours the configured format, and
// synthesizePCM, which forces pcm so it can chunk the result -- go through it,
// so the request shape is written once. The SDK hands back the *http.Response
// unread, which is what this endpoint needs: the body is audio, not JSON.
func (p *openAITTSProvider) speak(ctx context.Context, text, voice, responseFormat string) ([]byte, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("TTS provider is not configured")
	}
	resp, err := p.client.Audio.Speech.New(ctx, openai.AudioSpeechNewParams{
		Model:          openai.SpeechModel(p.model),
		Input:          text,
		Voice:          openai.AudioSpeechNewParamsVoiceUnion{OfString: openai.String(voice)},
		ResponseFormat: openai.AudioSpeechNewParamsResponseFormat(responseFormat),
		Speed:          openai.Float(numberParam(p.params["speed"], 1.0)),
	})
	if err != nil {
		return nil, fmt.Errorf("TTS request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("TTS request failed with status %d: %s", resp.StatusCode, string(body))
	}

	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read TTS response: %w", err)
	}
	return audioBytes, nil
}

// Call is not the primary interface for TTS - use Synthesize instead.
// This implementation synthesizes speech and returns metadata.
func (p *openAITTSProvider) Call(ctx context.Context, prompt string) (any, error) {
	voice := stringParam(p.params["voice"], "nova")
	audio, err := p.Synthesize(ctx, prompt, voice)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"audioSize": len(audio),
		"model":     p.model,
		"voice":     voice,
	}, nil
}

// Synthesize converts text to audio using OpenAI's TTS API.
func (p *openAITTSProvider) Synthesize(ctx context.Context, text string, voice string) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("TTS provider is not configured")
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("text is required for TTS")
	}
	if voice == "" {
		voice = stringParam(p.params["voice"], "nova")
	}

	audio, err := p.speak(ctx, text, voice, stringParam(p.params["format"], "pcm"))
	if err != nil {
		return nil, fmt.Errorf("failed to read TTS audio response: %w", err)
	}

	return audio, nil
}

// SynthesizeStream converts text to audio and streams chunks via channel.
// Uses PCM format internally and wraps each chunk in a WAV header for browser decode.
// Each chunk is independently decodable by browsers using decodeAudioData().
func (p *openAITTSProvider) SynthesizeStream(ctx context.Context, text string, voice string) (<-chan TTSChunk, error) {
	chunkCh := make(chan TTSChunk, 10)

	go func() {
		defer close(chunkCh)

		// Get PCM audio from OpenAI (we override format to pcm for reliable chunking)
		pcmData, err := p.synthesizePCM(ctx, text, voice)
		if err != nil {
			chunkCh <- TTSChunk{Error: err, Done: true}
			return
		}

		// Create WAV chunks from PCM data
		// Each chunk is a complete WAV file that browsers can decode independently
		p.streamWAVChunks(ctx, pcmData, chunkCh)
	}()

	return chunkCh, nil
}

// synthesizePCM requests PCM audio from OpenAI TTS API.
func (p *openAITTSProvider) synthesizePCM(ctx context.Context, text string, voice string) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("TTS provider is not configured")
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("text is required for TTS")
	}
	if voice == "" {
		voice = stringParam(p.params["voice"], "nova")
	}

	// Always request PCM for reliable chunking, whatever the configured format.
	return p.speak(ctx, text, voice, "pcm")
}

// streamWAVChunks splits PCM audio into independently decodable WAV chunks.
// Each chunk is a complete WAV file that browsers can decode with decodeAudioData().
func (p *openAITTSProvider) streamWAVChunks(ctx context.Context, pcmData []byte, chunkCh chan<- TTSChunk) {
	// OpenAI TTS PCM format: 24kHz, mono, 16-bit
	const (
		sampleRate    = 24000
		numChannels   = 1
		bitsPerSample = 16
	)

	chunks := audio.ChunkPCMToWAV(pcmData, sampleRate, numChannels, bitsPerSample, ttsTargetChunkDurationMS)

	for i, chunk := range chunks {
		select {
		case <-ctx.Done():
			chunkCh <- TTSChunk{Error: ctx.Err(), Done: true}
			return
		case chunkCh <- TTSChunk{
			Audio:    chunk.Data,
			Sequence: i,
			Done:     i == len(chunks)-1,
		}:
		}
	}
}

// Ensure openAITTSProvider implements TTSAIProvider
var _ TTSAIProvider = (*openAITTSProvider)(nil)

// ============================================================================
// Helper functions
// ============================================================================

func stringParam(value any, fallback string) string {
	if s, ok := value.(string); ok && s != "" {
		return s
	}
	return fallback
}

func numberParam(value any, fallback float64) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		if parsed, err := v.Float64(); err == nil {
			return parsed
		}
	}
	return fallback
}

// intParam reads a numeric provider param.
//
// SATURATES out of range (memql#4779). This is the widest-fanout site in the
// sweep -- some thirty call sites, nearly all of them `maxTokens` /
// `maxCompletionTokens` / `thinkingBudgetTokens` on their way to a paid API.
//
// Reporting ok=false instead was the tempting alternative and is the wrong
// one: the callers are `else if` chains that fall through to the provider's
// own default, so an absurd budget would be silently ignored and the request
// would succeed having spent whatever the vendor felt like. A saturated value
// is refused by every provider with a 400 naming the field, which is a loud
// failure about the number that was actually configured.
func intParam(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return num.ClampFloat64(v), true
	case float32:
		return num.ClampFloat64(float64(v)), true
	case int:
		return v, true
	case int64:
		return num.ClampInt64(v), true
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return num.ClampInt64(parsed), true
		}
	}
	return 0, false
}

// boolParam reads a boolean flag from a provider config params map.
// Accepts native bool or the string forms "true" / "false"
// (case-insensitive, whitespace-trimmed) -- operator configs
// often come from YAML where booleans can be quoted. Returns
// false for anything else.
func boolParam(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		trimmed := strings.ToLower(strings.TrimSpace(v))
		return trimmed == "true" || trimmed == "1" || trimmed == "yes" || trimmed == "on"
	}
	return false
}
