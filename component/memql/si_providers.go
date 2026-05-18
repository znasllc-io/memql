package memql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/sashabaranov/go-openai"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/audio"
)

const envDefaultProvider = "MEMQL_DEFAULT_PROVIDER"

// Variable names for default provider configuration.
// These are stored in v1:platform:partitionVariable and resolved at runtime.
const (
	VarDefaultTTSProvider    = "MEMQL_DEFAULT_TTS_PROVIDER"
	VarDefaultStreamProvider = "MEMQL_DEFAULT_STREAM_PROVIDER"
	VarDefaultChatProvider   = "MEMQL_DEFAULT_CHAT_PROVIDER"
)

// ProviderModality defines the input/output mode of a provider.
type ProviderModality string

const (
	ModalityText ProviderModality = "text" // Text input → text output (chat, stream)
	ModalityTTS  ProviderModality = "tts"  // Text input → audio output (text-to-speech)
	ModalitySTT       ProviderModality = "stt"       // Audio input → text output (speech-to-text)
	ModalityEmbedding ProviderModality = "embedding" // Text input → vector output (embeddings)
	// The following modalities are declared so provider .memql files can
	// register intent; their handlers are placeholders today (newSIProvider
	// routes them through newOpenAIPlaceholderProvider, which validates
	// auth and returns codes.Unimplemented on invocation). Add real
	// client implementations as capabilities come online; the .memql
	// files stay untouched when that happens.
	ModalityRealtime    ProviderModality = "realtime"    // Bidirectional audio + text over WebSocket
	ModalityAudio       ProviderModality = "audio"       // Single-shot audio in/out (unified)
	ModalityImage       ProviderModality = "image"       // Text input → image output
	ModalityVideo       ProviderModality = "video"       // Text input → video output
	ModalityComputerUse ProviderModality = "computerUse" // Agentic control of a virtual machine
	ModalityModeration  ProviderModality = "moderation"  // Text/image input → safety classifications
	ModalitySearch      ProviderModality = "search"      // Web-search-grounded chat
	ModalityResearch    ProviderModality = "research"    // Long-form deep-research pipelines
)

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
}

// ContextWindow returns the context window size from provider params,
// falling back to 0 if not explicitly set. Callers should use
// polyphon.LookupContextWindow(model) as a fallback for unknown values.
func (c ProviderConfig) ContextWindow() int {
	if cw, ok := c.Params["contextWindow"]; ok {
		switch v := cw.(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

// ResolvedModality returns the effective modality, inferring from Type if not explicitly set.
func (c ProviderConfig) ResolvedModality() ProviderModality {
	if c.Modality != "" {
		return ProviderModality(strings.ToLower(c.Modality))
	}
	// Infer from provider type
	switch strings.ToLower(c.Type) {
	case "openaitts":
		return ModalityTTS
	case "openaistt", "openaiwhisper":
		return ModalitySTT
	case "openaiembedding":
		return ModalityEmbedding
	case "openairealtime":
		return ModalityRealtime
	case "openaiaudio":
		return ModalityAudio
	case "openaiimage":
		return ModalityImage
	case "openaivideo":
		return ModalityVideo
	case "openaicomputeruse":
		return ModalityComputerUse
	case "openaimoderation":
		return ModalityModeration
	case "openaisearch":
		return ModalitySearch
	case "openaideepresearch":
		return ModalityResearch
	case "anthropic", "anthropicchat", "anthropicstream":
		return ModalityText
	case "google", "googlechat", "googlestream",
		"groq", "groqchat", "groqstream",
		"xai", "xaichat", "xaistream",
		"mistral", "mistralchat", "mistralstream":
		return ModalityText
	default:
		return ModalityText
	}
}

// SupportsText returns true if this provider can be used for text-based si() calls.
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

// SIProvider executes prompts against a backing model.
type SIProvider interface {
	Call(ctx context.Context, prompt string) (any, error)
}

// StreamingSIProvider extends SIProvider with streaming support.
type StreamingSIProvider interface {
	SIProvider
	CallStream(ctx context.Context, prompt string) (<-chan StreamChunk, error)
}

// RealtimeSIProvider extends SIProvider for WebSocket-based realtime interactions.
type RealtimeSIProvider interface {
	SIProvider
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

// TTSSIProvider provides text-to-speech synthesis capabilities.
type TTSSIProvider interface {
	SIProvider
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
	mu              sync.RWMutex
	byName          map[string]*ProviderConfigEntry
	defaultProvider string
	// defaultPinned is true when MEMQL_DEFAULT_CHAT_PROVIDER was set at
	// construction time; in that case no @default annotation or
	// first-wins fallback is allowed to override the operator's choice.
	defaultPinned bool
}

// ProviderConfigEntry stores metadata + instantiated client for a provider.
type ProviderConfigEntry struct {
	Config    ProviderConfig
	Client    SIProvider
	Available bool
	err       error
}

func newProviderRegistry(defaultName string) *ProviderRegistry {
	pinned := strings.TrimSpace(defaultName) != ""
	return &ProviderRegistry{
		byName:          make(map[string]*ProviderConfigEntry),
		defaultProvider: strings.TrimSpace(defaultName),
		defaultPinned:   pinned,
	}
}

func (r *ProviderRegistry) setEntry(entry *ProviderConfigEntry) {
	if r == nil || entry == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byName[strings.TrimSpace(entry.Config.Name)] = entry

	// Precedence when choosing the runtime default:
	//   1. MEMQL_DEFAULT_CHAT_PROVIDER env var (defaultPinned=true): never overridden.
	//   2. A provider marked @default: always wins over a prior first-wins fallback.
	//   3. First available registered provider: only used when nothing else applies.
	if r.defaultPinned {
		return
	}
	if entry.Config.Default && entry.Available {
		r.defaultProvider = entry.Config.Name
		return
	}
	if r.defaultProvider == "" && entry.Available {
		r.defaultProvider = entry.Config.Name
	}
}

func (r *ProviderRegistry) finalizeDefault(logger *slog.Logger) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.defaultProvider != "" {
		if entry, ok := r.byName[r.defaultProvider]; ok && entry != nil && entry.Available {
			return
		}
		if logger != nil {
			logger.Warn("default SI provider unavailable; selecting first available entry",
				"component", ComponentName,
				"provider", r.defaultProvider)
		}
		r.defaultProvider = ""
	}
	for name, entry := range r.byName {
		if entry != nil && entry.Available {
			r.defaultProvider = name
			break
		}
	}
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

// Default returns the preferred provider name, if any.
func (r *ProviderRegistry) Default() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultProvider
}

// Entry retrieves a provider configuration entry by name.
func (r *ProviderRegistry) Entry(name string) (*ProviderConfigEntry, bool) {
	if r == nil {
		return nil, false
	}
	key := strings.TrimSpace(name)
	if key == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byName[key]
	return entry, ok
}

// providerByName looks up an available named entry, applies an optional
// extra predicate, and type-asserts the Client to T. The {*T, false}
// zero-form covers every "no fit" path: name missing, entry
// unavailable, predicate rejected, or type assertion failed. Used by
// every *ProviderByName accessor.
func providerByName[T any](r *ProviderRegistry, name string, extra func(*ProviderConfigEntry) bool) (T, bool) {
	var zero T
	entry, ok := r.Entry(name)
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
func (r *ProviderRegistry) TTSProvider(defaultName string) TTSSIProvider {
	if r == nil {
		return nil
	}
	if defaultName != "" {
		if tts, ok := r.TTSProviderByName(defaultName); ok {
			return tts
		}
	}
	return providerScan[TTSSIProvider](r, ModalityTTS, nil)
}

// TTSProviderByName returns a specific TTS provider by name.
func (r *ProviderRegistry) TTSProviderByName(name string) (TTSSIProvider, bool) {
	return providerByName[TTSSIProvider](r, name, func(e *ProviderConfigEntry) bool {
		return e.Config.ResolvedModality() == ModalityTTS
	})
}

// ChatProvider returns a non-streaming chat provider suitable for synchronous SI calls
// (e.g., suggest endpoints). If defaultName is provided and exists, returns that;
// otherwise returns the first available non-streaming chat provider, or nil if none configured.
//
// This method explicitly excludes streaming providers (OpenAIStream, AnthropicStream)
// because their CallChat() implementation may target model endpoints that reject
// synchronous chat completions requests. Use StreamProvider() for streaming use cases.
func (r *ProviderRegistry) ChatProvider(defaultName string) common.ChatSIProvider {
	if r == nil {
		return nil
	}

	// Try the specified default first
	if defaultName != "" {
		if entry, ok := r.Entry(defaultName); ok && entry.Available {
			if cp, ok := entry.Client.(common.ChatSIProvider); ok && isNonStreamingType(entry.Config.Type) {
				return cp
			}
		}
	}

	// Try preferred provider names in priority order.
	// These are stable, production-ready models known to work with /v1/chat/completions.
	// Using an explicit list avoids random Go map iteration picking incompatible models
	// (e.g., codex models, deep-research / reasoning models that use /v1/responses
	// instead, or provider-type stubs that aren't wired to a real client).
	// Order: flagship mid-tier → mini → nano → pro → chat-latest alias.
	preferredNames := []string{"chat54", "chat54Mini", "chat54Nano", "chat54Pro", "chat53Latest"}
	for _, name := range preferredNames {
		if entry, ok := r.Entry(name); ok && entry.Available {
			if cp, ok := entry.Client.(common.ChatSIProvider); ok && isNonStreamingType(entry.Config.Type) {
				return cp
			}
		}
	}

	// Last resort: iterate all providers, filtering strictly
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, entry := range r.byName {
		if entry != nil && entry.Available && entry.Config.ResolvedModality() == ModalityText && isNonStreamingType(entry.Config.Type) && isChatCompatibleModel(entry.Config.Model) {
			if cp, ok := entry.Client.(common.ChatSIProvider); ok {
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
	// Preferred models, same priority order as ChatProvider.
	preferredNames := []string{"chat54", "chat54Mini", "chat54Nano", "chat54Pro", "chat53Latest"}
	for _, preferred := range preferredNames {
		if entry, ok := r.Entry(preferred); ok && entry.Available {
			if cp, ok := entry.Client.(common.ChatStructuredProvider); ok && isNonStreamingType(entry.Config.Type) {
				return cp
			}
		}
	}
	// Last resort: any structured-capable non-streaming provider.
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
func (r *ProviderRegistry) ChatStructuredProviderByName(name string) common.ChatStructuredProvider {
	if r == nil {
		return nil
	}
	cp, _ := providerByName[common.ChatStructuredProvider](r, strings.TrimSpace(name), func(e *ProviderConfigEntry) bool {
		return isNonStreamingType(e.Config.Type)
	})
	return cp
}

// SuggestChatProvider returns a fast, lightweight non-streaming chat provider
// optimized for suggestion endpoints. Prefers smaller/faster models over larger ones.
// Falls back to ChatProvider if no fast model is available.
func (r *ProviderRegistry) SuggestChatProvider() common.ChatSIProvider {
	if r == nil {
		return nil
	}

	// Prefer fast models for suggestions -- smaller models generate structured JSON quickly.
	// Nano is the cheapest; Mini is the balanced fallback; full 5.4 as a last resort.
	fastNames := []string{"chat54Nano", "chat54Mini", "chat54"}
	for _, name := range fastNames {
		if entry, ok := r.Entry(name); ok && entry.Available {
			if cp, ok := entry.Client.(common.ChatSIProvider); ok && isNonStreamingType(entry.Config.Type) {
				return cp
			}
		}
	}

	// Fall back to default chat provider
	return r.ChatProvider("")
}

// isNonStreamingType returns true for provider types that use synchronous (non-streaming)
// chat completions. These are safe for suggest/non-streaming SI calls.
func isNonStreamingType(providerType string) bool {
	switch strings.ToLower(providerType) {
	case "openai", "openaichat", "anthropic", "anthropicchat":
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
func (r *ProviderRegistry) StreamProvider(defaultName string) StreamingSIProvider {
	if r == nil {
		return nil
	}
	if defaultName != "" {
		if sp, ok := providerByName[StreamingSIProvider](r, defaultName, nil); ok {
			return sp
		}
	}
	return providerScan[StreamingSIProvider](r, ModalityText, nil)
}

// VisionProvider returns the first available provider that implements VisionSIProvider.
// If providerName is provided and exists, returns that; otherwise scans all text providers.
func (r *ProviderRegistry) VisionProvider(providerName string) common.VisionSIProvider {
	if r == nil {
		return nil
	}
	if providerName != "" {
		if vp, ok := providerByName[common.VisionSIProvider](r, providerName, nil); ok {
			return vp
		}
	}
	return providerScan[common.VisionSIProvider](r, ModalityText, nil)
}

// EmbeddingProvider returns a named embedding provider, or the first
// available one if name is empty. The named-lookup path preserves the
// legacy distinction between "not found" and "found but wrong type"
// errors -- callers grep on the error message in a few places.
func (r *ProviderRegistry) EmbeddingProvider(name string) (EmbeddingSIProvider, error) {
	if r == nil {
		return nil, fmt.Errorf("provider registry is nil")
	}

	if name != "" {
		r.mu.RLock()
		entry, ok := r.byName[name]
		r.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("embedding provider %q not found", name)
		}
		ep, ok := entry.Client.(EmbeddingSIProvider)
		if !ok {
			return nil, fmt.Errorf("provider %q is not an embedding provider (type: %s)", name, entry.Config.Type)
		}
		return ep, nil
	}

	if ep := providerScan[EmbeddingSIProvider](r, ModalityEmbedding, nil); ep != nil {
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

// loadSIProviders returns an empty registry. Pass 3 of the DSL
// restructure migration retired the legacy walk over
// dsl/v1/providers/. Providers now load via LoadUnifiedProviders
// (component/memql/unified_kinds_loader.go) which walks
// dsl/providers/<vendor>.memql.
func loadSIProviders(logger *slog.Logger) (*ProviderRegistry, error) {
	defaultName := strings.TrimSpace(os.Getenv(envDefaultProvider))
	registry := newProviderRegistry(defaultName)
	registry.finalizeDefault(logger)
	return registry, nil
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

// authConceptLookupNames returns the names to try in concept storage
// for a given placeholder name, in priority order. Providers historically
// reference `MEMQL_SI_<VENDOR>_API_KEY` (e.g. `MEMQL_SI_OPENAI_API_KEY`)
// because that's the OS env var the bridge-agent / STT bootstrap also
// reads. The dev workflow seeds the bare form (`OPENAI_API_KEY`,
// `ANTHROPIC_API_KEY`, ...) because that's what engineers know.
//
// Rather than rename one side or the other -- which would force every
// install to re-seed -- the resolver tries both:
//
//	MEMQL_SI_OPENAI_API_KEY  (exact match for what the provider asked)
//	OPENAI_API_KEY           (the dev-manifest seeded form)
//
// First non-empty match wins. Names that don't carry the prefix are
// looked up verbatim (no synthesized fallback).
func authConceptLookupNames(envKey string) []string {
	const elidedPrefix = "MEMQL_SI_"
	if strings.HasPrefix(envKey, elidedPrefix) {
		return []string{envKey, strings.TrimPrefix(envKey, elidedPrefix)}
	}
	return []string{envKey}
}

// authConceptResolver returns the value of a named entry in
// v1:platform:globalSecret first, then v1:platform:globalVariable. Encrypted /
// plaintext rows can both stand in for an auth field -- API keys are
// secrets, baseURL is a variable. Returns ("", false) if neither
// concept resolves the name (under any of the candidate names from
// authConceptLookupNames).
func authConceptResolver(envKey string) (string, bool) {
	ctx := context.Background()
	candidates := authConceptLookupNames(envKey)
	if systemSecretResolver != nil {
		for _, name := range candidates {
			if v, err := systemSecretResolver(ctx, name); err == nil {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed, true
				}
			}
		}
	}
	if systemVariableResolver != nil {
		for _, name := range candidates {
			if v, err := systemVariableResolver(ctx, name); err == nil {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					return trimmed, true
				}
			}
		}
	}
	return "", false
}

// resolveAuthPlaceholders walks a provider's auth map and substitutes
// any "${VAR_NAME}" placeholders with concrete values. Resolution
// order:
//
//  1. v1:platform:globalSecret    -- via systemSecretResolver, trying both
//                              the prefixed and bare-name forms (see
//                              authConceptLookupNames).
//  2. v1:platform:globalVariable  -- same, for non-sensitive auth fields
//                              like baseURL.
//  3. OS env                -- bootstrap-window fallback. Providers
//                              load eagerly during engine init, but
//                              `make dev-refresh` wipes the database
//                              before re-seeding -- so on first boot
//                              concept storage is empty when
//                              providers load, and only the env keeps
//                              them alive until the seed completes.
//                              Retiring this requires either
//                              lazy/per-request provider auth
//                              resolution or a post-seed engine
//                              reload; tracked as future work.
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
			// 1+2: concept storage (preferred).
			candidates := authConceptLookupNames(envKey)
			if v, ok := authConceptResolver(envKey); ok {
				resolved[key] = v
				continue
			}
			// 3: OS env fallback. Same prefix-elision as concept storage so
			// `OPENAI_API_KEY` in env wins for `MEMQL_SI_OPENAI_API_KEY`-
			// referencing providers, matching the dev-manifest naming.
			envHit := ""
			for _, name := range candidates {
				if v := strings.TrimSpace(os.Getenv(name)); v != "" {
					envHit = v
					break
				}
			}
			if envHit != "" {
				resolved[key] = envHit
				continue
			}
			return nil, fmt.Errorf(
				"auth %q references %s but no value is in concept storage or OS env. Tried name(s) %s under v1:platform:globalSecret, v1:platform:globalVariable, and the process env. Seed it with `make secret-set NAME=%s VALUE=... SCOPE=global` (or `variable-set` for non-sensitive values)",
				key, envKey, strings.Join(candidates, ", "), candidates[len(candidates)-1])
		}
		if trimmed == "" {
			return nil, fmt.Errorf("auth value %q is empty", key)
		}
		resolved[key] = trimmed
	}
	return resolved, nil
}

// streamingHTTPClient returns an HTTP client tuned for SI streaming
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
	return &http.Client{Transport: base}
}

func newSIProvider(cfg ProviderConfig) (SIProvider, error) {
	switch strings.ToLower(cfg.Type) {
	case "openai", "openaichat":
		return newOpenSIProvider(cfg)
	case "openaistream":
		return newOpenAIStreamProvider(cfg)
	// OpenAI-wire-compatible vendors. Each provider .memql sets its own
	// auth.baseURL and auth.apiKey env var; the existing OpenAI client
	// handles the wire format. Add a new vendor here by dropping a
	// provider/v1/<vendor>/_base.memql with the right baseURL and a
	// case entry below -- no new Go client required.
	case "google", "googlechat",
		"groq", "groqchat",
		"xai", "xaichat",
		"mistral", "mistralchat":
		return newOpenSIProvider(cfg)
	case "googlestream", "groqstream", "xaistream", "mistralstream":
		return newOpenAIStreamProvider(cfg)
	case "openaitts":
		return newOpenAITTSProvider(cfg)
	case "openaiembedding":
		return newOpenAIEmbeddingProvider(cfg)
	case "anthropic", "anthropicchat":
		return newAnthropicProvider(cfg)
	case "anthropicstream":
		return newAnthropicStreamProvider(cfg)
	// Placeholder types. These provider .memql files declare the model +
	// auth today; when the real Go client lands we swap the dispatch
	// case here to a concrete newXxxProvider(cfg) call. Until then the
	// stub validates auth and returns codes.Unimplemented on Call().
	case "openaistt", "openaiwhisper":
		return newOpenAIPlaceholderProvider(cfg, "stt")
	case "openairealtime":
		return newOpenAIPlaceholderProvider(cfg, "realtime")
	case "openaiaudio":
		return newOpenAIPlaceholderProvider(cfg, "audio")
	case "openaiimage":
		return newOpenAIPlaceholderProvider(cfg, "image")
	case "openaivideo":
		return newOpenAIPlaceholderProvider(cfg, "video")
	case "openaicomputeruse":
		return newOpenAIPlaceholderProvider(cfg, "computer-use")
	case "openaimoderation":
		return newOpenAIPlaceholderProvider(cfg, "moderation")
	case "openaisearch":
		return newOpenAIPlaceholderProvider(cfg, "search")
	case "openaideepresearch":
		return newOpenAIPlaceholderProvider(cfg, "deep-research")
	default:
		return nil, fmt.Errorf("unsupported provider type %q", cfg.Type)
	}
}

// openAIPlaceholderProvider stands in for capabilities that have a
// declared provider (.memql) but no Go client wired up yet. It
// validates auth at registration time so misconfigurations surface
// early, and returns codes.Unimplemented when someone actually calls
// it so the failure mode is obvious. When a real client is added,
// swap the dispatch case in newSIProvider and delete this provider's
// use for that type.
type openAIPlaceholderProvider struct {
	name       string
	model      string
	capability string // human-readable capability tag for error messages
}

func newOpenAIPlaceholderProvider(cfg ProviderConfig, capability string) (SIProvider, error) {
	if strings.TrimSpace(cfg.Auth["apiKey"]) == "" {
		return nil, fmt.Errorf("provider %q (%s): missing auth.apiKey", cfg.Name, capability)
	}
	return &openAIPlaceholderProvider{
		name:       cfg.Name,
		model:      cfg.Model,
		capability: capability,
	}, nil
}

// Call satisfies SIProvider. Returns an informative error rather than
// pretending to succeed -- we never want a placeholder to silently
// absorb a real request.
func (p *openAIPlaceholderProvider) Call(_ context.Context, _ string) (any, error) {
	return nil, fmt.Errorf(
		"provider %q (%s / model=%s) is declared but the Go client is not wired yet; "+
			"add a dispatch case in component/memql/si_providers.go:newSIProvider",
		p.name, p.capability, p.model,
	)
}

// ============================================================================
// OpenAI Embedding Provider
// ============================================================================

func newOpenAIEmbeddingProvider(cfg ProviderConfig) (SIProvider, error) {
	apiKey := strings.TrimSpace(cfg.Auth["apiKey"])
	if apiKey == "" {
		return nil, fmt.Errorf("provider %q missing auth.apiKey", cfg.Name)
	}
	dims := 1536
	if d, ok := cfg.Params["dimensions"]; ok {
		switch v := d.(type) {
		case float64:
			dims = int(v)
		case int:
			dims = v
		}
	}
	return NewOpenAIEmbeddingClient(apiKey, cfg.Model, dims), nil
}

// ============================================================================
// OpenAI Chat Provider (Standard non-streaming)
// ============================================================================

// openAIProjectHeaderClient wraps an HTTP client to inject the OpenAI-Project header.
type openAIProjectHeaderClient struct {
	inner     openai.HTTPDoer
	projectId string
}

func (c *openAIProjectHeaderClient) Do(req *http.Request) (*http.Response, error) {
	req.Header.Set("OpenAI-Project", c.projectId)
	return c.inner.Do(req)
}

// resolveOpenAIProjectId returns the OpenAI-Project header value for a
// provider config, preferring auth.projectId when the caller set it in
// a .memql override, and falling back to MEMQL_SI_OPENAI_PROJECT_ID
// from the environment. Returns "" when neither is set -- which is the
// steady state for service-account keys (sk-svcacct-*), which do not
// carry a project id. The openai-go client handles the empty case
// transparently (the header is simply not attached).
func resolveOpenAIProjectId(cfg ProviderConfig) string {
	if pid := strings.TrimSpace(cfg.Auth["projectId"]); pid != "" {
		return pid
	}
	return strings.TrimSpace(os.Getenv("MEMQL_SI_OPENAI_PROJECT_ID"))
}

type openSIProvider struct {
	client *openai.Client
	model  string
	params map[string]any
}

// Compile-time interface assertions
var _ SIProvider = (*openSIProvider)(nil)
var _ common.ChatSIProvider = (*openSIProvider)(nil)
var _ common.ToolCallingChatSIProvider = (*openSIProvider)(nil)
var _ common.ChatStructuredProvider = (*openSIProvider)(nil)
var _ common.VisionSIProvider = (*openSIProvider)(nil)

func newOpenSIProvider(cfg ProviderConfig) (SIProvider, error) {
	apiKey := strings.TrimSpace(cfg.Auth["apiKey"])
	if apiKey == "" {
		return nil, fmt.Errorf("provider %q missing auth.apiKey", cfg.Name)
	}
	config := openai.DefaultConfig(apiKey)
	if baseURL := strings.TrimSpace(cfg.Auth["baseURL"]); baseURL != "" {
		config.BaseURL = baseURL
	}
	if projectId := resolveOpenAIProjectId(cfg); projectId != "" {
		config.HTTPClient = &openAIProjectHeaderClient{inner: config.HTTPClient, projectId: projectId}
	}
	client := openai.NewClientWithConfig(config)
	return &openSIProvider{
		client: client,
		model:  cfg.Model,
		params: cfg.Params,
	}, nil
}

func (p *openSIProvider) Call(ctx context.Context, prompt string) (any, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai provider is not configured")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	req := openai.ChatCompletionRequest{
		Model: p.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}

	// Support both parameter names for compatibility
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
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

// CallChat implements ChatSIProvider for proper multi-turn conversations.
// This sends messages with correct roles (system/user/assistant) to OpenAI,
// which is critical for the model to understand conversation context.
func (p *openSIProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("openai provider is not configured")
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openAIMessages,
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
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
func (p *openSIProvider) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("openai provider is not configured")
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("at least one message is required")
	}
	if len(schema.Schema) == 0 {
		return "", fmt.Errorf("structured chat requires a non-empty schema")
	}
	name := strings.TrimSpace(schema.Name)
	if name == "" {
		name = "response"
	}

	openAIMessages := toOpenAIChatMessages(messages)

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openAIMessages,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:        name,
				Description: schema.Description,
				Schema:      json.RawMessage(schema.Schema),
				Strict:      schema.Strict,
			},
		},
	}

	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned")
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
			return "", fmt.Errorf("model refused structured output: %s", refusal)
		}
		switch choice.FinishReason {
		case openai.FinishReasonLength:
			return "", fmt.Errorf(
				"empty content with finish_reason=length (model %q hit completion-token cap before producing output -- raise maxCompletionTokens or shrink the schema/input)",
				p.model,
			)
		case openai.FinishReasonContentFilter:
			return "", fmt.Errorf("empty content with finish_reason=content_filter (response was suppressed by safety filter)")
		case "":
			return "", fmt.Errorf("empty content with no finish_reason returned (model %q may not exist or returned a malformed response)", p.model)
		default:
			return "", fmt.Errorf("empty content with finish_reason=%s (model %q)", choice.FinishReason, p.model)
		}
	}
	return content, nil
}

// CallChatWithTools implements tool-calling chat completion using OpenAI tools.
func (p *openSIProvider) CallChatWithTools(ctx context.Context, messages []common.ChatMessage, tools []common.ToolDefinition) (*common.ToolCallingChatResult, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai provider is not configured")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)
	openAITools := toOpenAITools(tools)

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openAIMessages,
		Tools:    openAITools,
		ToolChoice: "auto",
		// Prefer sequential tool calls for determinism/simplicity.
		ParallelToolCalls: false,
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
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

// CallVision implements common.VisionSIProvider for the OpenAI provider.
func (p *openSIProvider) CallVision(ctx context.Context, prompt string, images []common.VisionContent) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("openai provider is not configured")
	}
	if len(images) == 0 {
		return "", fmt.Errorf("at least one image is required")
	}

	parts := make([]openai.ChatMessagePart, 0, len(images)+1)
	for _, img := range images {
		encoded := base64.StdEncoding.EncodeToString(img.Data)
		dataURI := fmt.Sprintf("data:%s;base64,%s", img.MimeType, encoded)
		parts = append(parts, openai.ChatMessagePart{
			Type: openai.ChatMessagePartTypeImageURL,
			ImageURL: &openai.ChatMessageImageURL{
				URL:    dataURI,
				Detail: openai.ImageURLDetailAuto,
			},
		})
	}
	parts = append(parts, openai.ChatMessagePart{
		Type: openai.ChatMessagePartTypeText,
		Text: prompt,
	})

	req := openai.ChatCompletionRequest{
		Model: p.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, MultiContent: parts},
		},
	}
	if maxTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	} else {
		req.MaxCompletionTokens = 1000
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
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

func newOpenAIStreamProvider(cfg ProviderConfig) (SIProvider, error) {
	apiKey := strings.TrimSpace(cfg.Auth["apiKey"])
	if apiKey == "" {
		return nil, fmt.Errorf("provider %q missing auth.apiKey", cfg.Name)
	}
	config := openai.DefaultConfig(apiKey)
	if baseURL := strings.TrimSpace(cfg.Auth["baseURL"]); baseURL != "" {
		config.BaseURL = baseURL
	}
	// Tune the HTTP client BEFORE the project-header wrap so the
	// projectId layer composes over the timeout-tuned transport.
	config.HTTPClient = streamingHTTPClient()
	if projectId := resolveOpenAIProjectId(cfg); projectId != "" {
		config.HTTPClient = &openAIProjectHeaderClient{inner: config.HTTPClient, projectId: projectId}
	}
	client := openai.NewClientWithConfig(config)
	return &openAIStreamProvider{
		client: client,
		model:  cfg.Model,
		params: cfg.Params,
	}, nil
}

// Call implements SIProvider with non-streaming fallback for compatibility.
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

// CallChat implements ChatSIProvider for proper multi-turn conversations.
func (p *openAIStreamProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	if p == nil || p.client == nil {
		return "", fmt.Errorf("openai stream provider is not configured")
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("at least one message is required")
	}

	openAIMessages := toOpenAIChatMessages(messages)

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openAIMessages,
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
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

	req := openai.ChatCompletionRequest{
		Model:             p.model,
		Messages:          openAIMessages,
		Tools:             openAITools,
		ToolChoice:        "auto",
		ParallelToolCalls: false,
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
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

func toOpenAITools(tools []common.ToolDefinition) []openai.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openai.Tool, 0, len(tools))
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
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        name,
				Description: strings.TrimSpace(t.Description),
				Parameters:  params,
			},
		})
	}
	return out
}

func toOpenAIChatMessages(messages []common.ChatMessage) []openai.ChatCompletionMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionMessage, len(messages))
	for i, msg := range messages {
		role := openai.ChatMessageRoleUser
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system":
			role = openai.ChatMessageRoleSystem
		case "assistant":
			role = openai.ChatMessageRoleAssistant
		case "user":
			role = openai.ChatMessageRoleUser
		case "tool":
			role = openai.ChatMessageRoleTool
		}
		var toolCalls []openai.ToolCall
		if len(msg.ToolCalls) > 0 {
			toolCalls = make([]openai.ToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(tc.Name) == "" {
					continue
				}
				toolCalls = append(toolCalls, openai.ToolCall{
					ID:   strings.TrimSpace(tc.ID),
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      strings.TrimSpace(tc.Name),
						Arguments: strings.TrimSpace(tc.Arguments),
					},
				})
			}
		}
		out[i] = openai.ChatCompletionMessage{
			Role:       role,
			Content:    msg.Content,
			Name:       strings.TrimSpace(msg.Name),
			ToolCallID: strings.TrimSpace(msg.ToolCallId),
			ToolCalls:  toolCalls,
		}
	}
	return out
}

// CallStream implements StreamingSIProvider for streaming responses.
func (p *openAIStreamProvider) CallStream(ctx context.Context, prompt string) (<-chan StreamChunk, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("openai stream provider is not configured")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	req := openai.ChatCompletionRequest{
		Model: p.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
		Stream: true,
	}

	// Only set temperature/topP when explicitly configured (some models reject these params)
	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}

	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create stream: %w", err)
	}

	chunks := make(chan StreamChunk, 100)

	go func() {
		defer close(chunks)
		defer stream.Close()

		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				chunks <- StreamChunk{Done: true}
				return
			}
			if err != nil {
				chunks <- StreamChunk{Error: err, Done: true}
				return
			}

			if len(resp.Choices) > 0 && resp.Choices[0].Delta.Content != "" {
				chunks <- StreamChunk{Content: resp.Choices[0].Delta.Content}
			}
		}
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

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openAIMessages,
		Stream:   true,
	}

	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create chat stream: %w", err)
	}

	chunks := make(chan common.StreamChunk, 100)

	go func() {
		defer close(chunks)
		defer stream.Close()

		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				chunks <- common.StreamChunk{Done: true}
				return
			}
			if err != nil {
				chunks <- common.StreamChunk{Error: err, Done: true}
				return
			}

			if len(resp.Choices) > 0 && resp.Choices[0].Delta.Content != "" {
				chunks <- common.StreamChunk{Content: resp.Choices[0].Delta.Content}
			}
		}
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

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openAIMessages,
		Stream:   true,
	}
	if len(openAITools) > 0 {
		req.Tools = openAITools
		req.ToolChoice = "auto"
		// Let the model emit multiple sequential tool calls in one
		// response. Critical for multi-step takeovers (e.g. a CoPresent
		// Operator session = request + navigate + click + click +
		// release) where forcing one-per-response burns through the
		// streaming tool-loop iteration budget and the agent stalls
		// mid-sequence. The server-side loop executes parallel calls
		// in order and feeds all results back before the next turn.
		req.ParallelToolCalls = true
	}

	if _, ok := p.params["temperature"]; ok {
		req.Temperature = float32(numberParam(p.params["temperature"], 0.0))
	}
	if _, ok := p.params["topP"]; ok {
		req.TopP = float32(numberParam(p.params["topP"], 1.0))
	}
	if maxCompletionTokens, ok := intParam(p.params["maxCompletionTokens"]); ok {
		req.MaxCompletionTokens = maxCompletionTokens
	} else if maxTokens, ok := intParam(p.params["maxTokens"]); ok {
		req.MaxCompletionTokens = maxTokens
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to create stream with tools: %w", err)
	}

	chunks := make(chan common.StreamToolChunk, 100)

	go func() {
		defer close(chunks)
		defer stream.Close()

		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				chunks <- common.StreamToolChunk{Done: true}
				return
			}
			if err != nil {
				chunks <- common.StreamToolChunk{Error: err, Done: true}
				return
			}
			if len(resp.Choices) == 0 {
				continue
			}

			delta := resp.Choices[0].Delta
			chunk := common.StreamToolChunk{Content: delta.Content}

			for _, tc := range delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				chunk.ToolCalls = append(chunk.ToolCalls, common.ToolCallDelta{
					Index:     idx,
					ID:        strings.TrimSpace(tc.ID),
					Name:      strings.TrimSpace(tc.Function.Name),
					Arguments: tc.Function.Arguments,
				})
			}

			if chunk.Content != "" || len(chunk.ToolCalls) > 0 {
				chunks <- chunk
			}
		}
	}()

	return chunks, nil
}

// Ensure openAIStreamProvider implements StreamingSIProvider, ChatStreamProvider, and ChatStreamWithToolsProvider
var _ StreamingSIProvider = (*openAIStreamProvider)(nil)
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
var _ SIProvider = (*anthropicProvider)(nil)
var _ common.ChatSIProvider = (*anthropicProvider)(nil)
var _ common.ToolCallingChatSIProvider = (*anthropicProvider)(nil)
var _ common.VisionSIProvider = (*anthropicProvider)(nil)

func newAnthropicProvider(cfg ProviderConfig) (SIProvider, error) {
	apiKey := strings.TrimSpace(cfg.Auth["apiKey"])
	if apiKey == "" {
		return nil, fmt.Errorf("provider %q missing auth.apiKey", cfg.Name)
	}
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
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

// CallVision implements common.VisionSIProvider for the Anthropic provider.
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
	client anthropic.Client
	model  string
	params map[string]any
}

// Compile-time interface assertions
var _ StreamingSIProvider = (*anthropicStreamProvider)(nil)
var _ common.ChatStreamProvider = (*anthropicStreamProvider)(nil)
var _ common.ChatStreamWithToolsProvider = (*anthropicStreamProvider)(nil)
var _ common.ChatSIProvider = (*anthropicStreamProvider)(nil)
var _ common.ToolCallingChatSIProvider = (*anthropicStreamProvider)(nil)

func newAnthropicStreamProvider(cfg ProviderConfig) (SIProvider, error) {
	apiKey := strings.TrimSpace(cfg.Auth["apiKey"])
	if apiKey == "" {
		return nil, fmt.Errorf("provider %q missing auth.apiKey", cfg.Name)
	}
	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithHTTPClient(streamingHTTPClient()),
	)
	return &anthropicStreamProvider{
		client: client,
		model:  cfg.Model,
		params: cfg.Params,
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

// CallChat implements ChatSIProvider (non-streaming fallback).
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

// CallChatWithTools implements ToolCallingChatSIProvider (non-streaming fallback).
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

// CallStream implements StreamingSIProvider for single-prompt streaming.
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
		for stream.Next() {
			event := stream.Current()
			if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" {
				chunks <- StreamChunk{Content: event.Delta.Text}
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

const (
	openAITTSEndpoint = "https://api.openai.com/v1/audio/speech"

	// Target duration per chunk for progressive decode (milliseconds)
	// 200ms provides good balance between latency and overhead
	ttsTargetChunkDurationMS = 200
)

type openAITTSProvider struct {
	apiKey string
	model  string
	params map[string]any
}

func newOpenAITTSProvider(cfg ProviderConfig) (SIProvider, error) {
	apiKey := strings.TrimSpace(cfg.Auth["apiKey"])
	if apiKey == "" {
		return nil, fmt.Errorf("provider %q missing auth.apiKey", cfg.Name)
	}

	return &openAITTSProvider{
		apiKey: apiKey,
		model:  cfg.Model,
		params: cfg.Params,
	}, nil
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

	speed := numberParam(p.params["speed"], 1.0)
	responseFormat := stringParam(p.params["format"], "pcm")

	reqBody := map[string]any{
		"model":           p.model,
		"input":           text,
		"voice":           voice,
		"response_format": responseFormat,
		"speed":           speed,
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TTS request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAITTSEndpoint, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create TTS request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("TTS request failed with status %d: %s", resp.StatusCode, string(body))
	}

	audio, err := io.ReadAll(resp.Body)
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

	speed := numberParam(p.params["speed"], 1.0)

	reqBody := map[string]any{
		"model":           p.model,
		"input":           text,
		"voice":           voice,
		"response_format": "pcm", // Always request PCM for reliable chunking
		"speed":           speed,
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal TTS request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAITTSEndpoint, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create TTS request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("TTS request failed with status %d: %s", resp.StatusCode, string(body))
	}

	pcmData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read TTS response: %w", err)
	}

	return pcmData, nil
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

// Ensure openAITTSProvider implements TTSSIProvider
var _ TTSSIProvider = (*openAITTSProvider)(nil)

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

func intParam(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		if parsed, err := v.Int64(); err == nil {
			return int(parsed), true
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
