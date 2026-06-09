// Package cognition provides the cognition integration for MemQL.
// It handles SI response generation for the cognition module by subscribing
// to utterance events and generating responses using configured SI providers.
package cognition

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/logger"
	"github.com/znasllc-io/memql/integrations"
	"golang.org/x/sync/singleflight"
)

// ComponentName identifies this package in logs and environment variable lookups.
const ComponentName = common.ComponentName("cognitionIntegration")

// MemQL variable names for Cognition configuration.
const (
	VarCognitionSIEnabled           = "MEMQL_COGNITION_SI_ENABLED"
	VarCognitionDefaultProvider     = "MEMQL_COGNITION_DEFAULT_PROVIDER"
	VarCognitionConversationHistory = "MEMQL_COGNITION_CONVERSATION_HISTORY_LIMIT"
)

// Event subscription patterns for the cognition integration.
// Post-#56 phase 8 the topic shape is graph.node.{action}.{concept}.
const (
	eventPatternSpaceCreated     = "graph.node.created.v1:cognition:space"
	eventPatternParticipantAdded = "graph.node.created.v1:cognition:participant"
	eventPatternSessionChanged   = "graph.node.created.v1:cognition:session"
	eventPatternUtteranceCreated = "graph.node.created.v1:cognition:utterance"
)

// VariableResolver resolves variable values from the v1:platform:partitionVariable concept.
type VariableResolver func(ctx context.Context, name string) (string, error)

// MemQLEngine provides the limited engine interface for protocol-level
// operations the cognition integration needs. Mirrors
// memql.IntegrationEngineAccess so an adapter that satisfies the canonical
// interface satisfies this one too.
type MemQLEngine interface {
	// Execute runs a MemQL query or function call and returns the result.
	// Used for calling registered MemQL functions and insert mutations.
	Execute(ctx context.Context, query string) (any, error)
	// InvokeSI runs a prompt template by ID with the given data map.
	// Used by the SI router and fallback responder for top-level SI
	// invocations that can't sit inside a shape() projection.
	InvokeSI(ctx context.Context, templateId string, data map[string]any) (any, error)
	// InvokeSIStructured renders a prompt template and invokes its
	// default chat provider with a JSON schema the model must match.
	// Returns the raw JSON string. Used for routing, classification,
	// suggestion -- any "logic" prompt where shape correctness matters
	// more than prose quality.
	InvokeSIStructured(
		ctx context.Context,
		templateId string,
		data map[string]any,
		schemaName string,
		schema json.RawMessage,
		strict bool,
	) (string, error)
	// RenderPrompt renders a prompt template with the given data and returns the text.
	// Used by streaming handlers to prepare system prompts.
	RenderPrompt(templateId string, data map[string]any) (string, error)
	// RegisterIntegration registers an IntegrationProvider with the engine.
	RegisterIntegration(provider memql.IntegrationProvider) error
	// ChatStreamProvider returns a streaming chat provider, or nil if unavailable.
	ChatStreamProvider() common.ChatStreamProvider
	// ChatStreamProviderByName returns a named streaming chat provider, or nil.
	ChatStreamProviderByName(name string) common.ChatStreamProvider
	// ChatStreamWithToolsProviderByName returns a named provider that supports
	// streaming chat with tools, or nil if unavailable.
	ChatStreamWithToolsProviderByName(name string) common.ChatStreamWithToolsProvider
	// ToolDefinitionsForNames returns tool definitions for the given tool names.
	ToolDefinitionsForNames(names []string) []common.ToolDefinition
	// ExecuteToolByName looks up a tool by name and executes it with the given args.
	ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error)
	// ResolveSkills returns the unioned (domain, tool, liveSource)
	// surface for a list of v1:agents:skill ids. Phase 2 cut (#158):
	// the canonical helper every consumer of the new agent shape
	// calls before reading capabilities.
	ResolveSkills(ctx context.Context, skillIds []string) (memql.SkillBundle, error)
}

// SIProviderRegistry provides access to SI providers.
type SIProviderRegistry interface {
	// GetProvider returns an SI provider by name.
	GetProvider(name string) (any, bool)
	// DefaultProvider returns the default SI provider.
	DefaultProvider() (any, bool)
}

type (
	// CognitionArg is a constructor argument for CognitionIntegration.
	CognitionArg common.CtorArg[cognitionConfig]

	// CognitionIntegration provides SI response generation for cognition spaces.
	CognitionIntegration struct {
		*integrations.Integration // Embed base integration

		variableResolver VariableResolver
		engine           MemQLEngine
		providerRegistry SIProviderRegistry
		eventBus         *events.Bus
		config           *cognitionConfig

		// Polyphon score engine — deterministic scoring engine for turn-taking decisions.
		scoreEngine *polyphon.ScoreEngine

		// Event subscription cleanup
		unsubscribes []func()

		// Hot-path prompt context caches (best-effort, TTL-based).
		promptCacheMu      sync.Mutex
		participantsCache  map[string]cachedParticipants
		spaceContextCache  map[string]cachedSpaceContext
		siParticipantCache map[string]cachedSIParticipant
		agentCache         map[string]cachedAgent
		spaceInfoCache     map[string]cachedSpaceInfo

		// singleflight groups to prevent cache-miss thundering herd.
		participantsSF  singleflight.Group
		spaceContextSF  singleflight.Group
		siParticipantSF singleflight.Group
		agentSF         singleflight.Group
		recentUtterSF   singleflight.Group
		spaceInfoSF     singleflight.Group
		attachmentsSF   singleflight.Group

		// Attachment summaries cache (per space), included in SI prompt context.
		attachmentsCache map[string]cachedAttachments

		// Best-effort recent utterance buffer (per space) to avoid DB reads on hot path.
		recentUtterMu    sync.Mutex
		recentUtterCache map[string][]map[string]any

		// Space context heartbeat (reconciliation)
		spaceContextHeartbeatCancel context.CancelFunc
		spaceContextHeartbeatDone   chan struct{}

		// embedFunc is an optional function that computes a vector embedding for text.
		// When set, the cognition integration embeds utterance text for vector-based domain scoring.
		// Best-effort: if it returns an error, keyword matching is used as fallback.
		embedFunc func(ctx context.Context, text, provider string) ([]float32, error)

		// classifier is the LLM-driven structured-output classifier
		// for user messages, with a hash-keyed in-memory TTL cache.
		// Replaces the hardcoded affirmation/follow-up suppression
		// guard. See message_classifier.go +
		// docs/internal/planning/llm-driven-decisions.md.
		classifier *messageClassifier

		// profileEmbeddings caches agent profile embeddings in memory (keyed by agent ID).
		// TTL: 10 minutes. Prevents recomputing embeddings on every utterance.
		profileEmbeddings   map[string]profileEmbeddingEntry
		profileEmbeddingsMu sync.RWMutex

		// reactiveDispatchAt tracks when the heartbeat-driven topic
		// listener last fired a reactive dispatch for a given
		// (space, agent, topic) tuple. The cooldown
		// (reactiveDispatchCooldown) prevents two consecutive
		// heartbeat ticks with the same prediction from stacking
		// duplicate investigations. Cleaned implicitly: entries age
		// out via the time-since check, no explicit eviction needed.
		reactiveDispatchAt map[string]time.Time
		reactiveDispatchMu sync.Mutex

		// greetingPacing serializes greetOnJoin SI greetings per
		// space so they don't all fire at the instant their
		// participant.created events arrive. Two pacing rules:
		//
		//   1. Initial delay -- the FIRST greeting in a space
		//      sleeps `initialGreetingDelay` after acquiring its
		//      slot, giving the user's frontend time to dismiss
		//      the create-agent / create-space modal, navigate to
		//      the new space, and render the chat surface so the
		//      greeting doesn't land before the user can see it.
		//
		//   2. Inter-greeting pace -- subsequent greetings sleep
		//      until at least `interGreetingPace` has elapsed
		//      since the previous greeting was inserted, so a
		//      space with multiple greetOnJoin agents reads as
		//      conversational instead of a chorus of simultaneous
		//      hellos.
		//
		// Per-space serialization is enforced by the entry's
		// internal mutex; two greeting goroutines for the same
		// space never run their LLM calls concurrently. The map
		// itself is mutex-guarded only on lookup/insert; we don't
		// reap entries because each is small and bounded by the
		// number of unique spaces ever observed in this process.
		greetingPacingMu sync.Mutex
		greetingPacing   map[string]*greetingPacingState

		// conductors holds the per-space ConductorState that drives
		// orchestration decisions (who speaks, in what mode, how
		// brief, with what content angle). Conductors live in
		// memory; created lazily on first observation; cleaned up
		// when the space is closed. See conductor.go for the full
		// design rationale.
		conductors *ConductorRegistry

		// latestHumanUtterance tracks the most recent human utterance ID
		// per space. The handler uses it as a debounce check: after a
		// short delay on entry, it verifies it's still the newest
		// utterance for that space. If a newer one has arrived in the
		// meantime, this handler exits -- the newer one will process
		// the full transcript including what this handler saw. Prevents
		// AIs from jumping in mid-thought when a human is sending
		// rapid-fire messages to finish a single idea.
		latestHumanUtteranceMu sync.Mutex
		latestHumanUtterance   map[string]string

		// agentForwarder routes AgentGenerateTurnMsg to an agent peer
		// via NodeService.SIForwardRequest. Injected by app bootstrap
		// on cognition binaries; nil otherwise (cognition is the only
		// node type that originates agent-turn forwards today).
		agentForwarder AgentForwarder

		// pendingClientToolCalls tracks in-flight client-tool calls
		// relayed across cluster nodes. When the agent node emits a
		// ClientToolCall (intercepted in consumeAgentTurnStream), we
		// record (callId -> correlation) here and insert a
		// v1:cognition:client:tool:request graph node so the browser
		// sees it. The matching v1:cognition:client:tool:response event
		// (handled by handleClientToolResponse) uses the entry to
		// forward a ClientToolResult envelope back to the agent via
		// ForwardContinuation. Entries self-expire to bound memory if
		// a browser never responds.
		pendingClientToolCalls sync.Map // callId -> *pendingClientToolCall

		// dispatchGate ensures exactly ONE cognition replica dispatches a
		// given utterance's turn, via a Postgres advisory lock keyed by the
		// utterance id (znasllc-io/memql#1217). The utterance-created event is
		// broadcast to every cognition replica, so without this gate both
		// replicas pass the read-before-write idempotency check and each emits
		// its own reply -> a duplicate. Fails SAFE (proceeds on any DB/lock
		// error) and is a no-op on single-replica. Built in the constructor;
		// nil-tolerant (a nil gate proceeds, matching fail-safe). See
		// dispatch_gate.go.
		dispatchGate *dispatchGate
	}

	// profileEmbeddingEntry caches a single agent's profile embedding.
	profileEmbeddingEntry struct {
		embedding []float32
		textHash  string // first 16 chars of SHA-256 of profile text
		cachedAt  time.Time
	}

	// cognitionConfig holds Cognition-specific configuration.
	// Note: Runtime configuration (SI enabled, provider, history limit) is fetched
	// from v1:platform:partitionVariable concept records via VariableResolver.
	cognitionConfig struct {
		variableResolver VariableResolver
		engine           MemQLEngine
		providerRegistry SIProviderRegistry
		eventBus         *events.Bus
		scoreEngine      *polyphon.ScoreEngine
		// dbGetter is a lazy *bun.DB accessor used by the dispatch gate's
		// Postgres advisory lock (znasllc-io/memql#1217). Optional: when nil
		// the gate fails safe (always proceeds), so single-binary / no-DB
		// builds are unaffected.
		dbGetter func() *bun.DB
	}
)

// NewLogger creates a logger for the Cognition integration component.
func NewLogger() *slog.Logger {
	return logger.New(ComponentName, os.Stdout, resolveLoggerLevel())
}

func resolveLoggerLevel() slog.Level {
	reader := env.NewEnvReader("COGNITION_INTEGRATION")
	for _, key := range loggerLevelKeys() {
		if value, ok := reader.String(key); ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			var level slog.Level
			if err := level.UnmarshalText([]byte(strings.ToLower(value))); err == nil {
				return level
			}
		}
	}
	return slog.LevelInfo
}

func loggerLevelKeys() []string {
	return []string{
		"CAPABILITIES_LOGGING_LOG_LEVEL",
		"LOGGER_LEVEL",
		"LOG_LEVEL",
	}
}

// defaultCognitionConfig returns the default Cognition configuration.
func defaultCognitionConfig() *cognitionConfig {
	return &cognitionConfig{}
}

// OptionalCognitionArg creates an optional constructor argument.
func OptionalCognitionArg(name string, fn func(*cognitionConfig)) CognitionArg {
	return common.NewCtorArg(name, true, fn)
}

// RequiredCognitionArg creates a required constructor argument.
func RequiredCognitionArg(name string, fn func(*cognitionConfig)) CognitionArg {
	return common.NewCtorArg(name, false, fn)
}

// WithVariableResolver sets the variable resolver for fetching configuration.
func WithVariableResolver(resolver VariableResolver) CognitionArg {
	return OptionalCognitionArg("variable_resolver", func(c *cognitionConfig) {
		c.variableResolver = resolver
	})
}

// WithEngine sets the MemQL engine for queries and mutations.
func WithEngine(engine MemQLEngine) CognitionArg {
	return RequiredCognitionArg("engine", func(c *cognitionConfig) {
		c.engine = engine
	})
}

// WithProviderRegistry sets the SI provider registry.
func WithProviderRegistry(registry SIProviderRegistry) CognitionArg {
	return RequiredCognitionArg("provider_registry", func(c *cognitionConfig) {
		c.providerRegistry = registry
	})
}

// WithEventBus sets the event bus for subscribing to events.
func WithEventBus(bus *events.Bus) CognitionArg {
	return RequiredCognitionArg("event_bus", func(c *cognitionConfig) {
		c.eventBus = bus
	})
}

// WithScoreEngine sets the Polyphon score engine for turn-taking decisions.
func WithScoreEngine(scoreEngine *polyphon.ScoreEngine) CognitionArg {
	return RequiredCognitionArg("score_engine", func(c *cognitionConfig) {
		c.scoreEngine = scoreEngine
	})
}

// WithDBGetter sets the lazy *bun.DB accessor used by the dispatch gate's
// Postgres advisory lock (znasllc-io/memql#1217). Optional -- the same lazy
// getter the cron leader uses (a.db.BunDB); nil-safe (a nil getter or nil DB
// makes the gate fail safe / no-op).
func WithDBGetter(dbGetter func() *bun.DB) CognitionArg {
	return OptionalCognitionArg("db_getter", func(c *cognitionConfig) {
		c.dbGetter = dbGetter
	})
}

// SetEmbedFunc sets the embedding function for vector-based domain scoring.
// When set, utterance text is embedded during multi-agent routing to enable
// semantic matching against agent profile embeddings.
func (c *CognitionIntegration) SetEmbedFunc(fn func(ctx context.Context, text, provider string) ([]float32, error)) {
	c.embedFunc = fn
}

// profileEmbeddingTTL is how long a cached agent profile embedding is valid.
const profileEmbeddingTTL = 10 * time.Minute

// getOrComputeProfileEmbedding returns a cached embedding for the agent's profile
// text, computing it on-demand if the cache is stale or missing. Returns nil on
// error or when embedFunc is not configured.
func (c *CognitionIntegration) getOrComputeProfileEmbedding(ctx context.Context, agentId, profileText string) []float32 {
	if c.embedFunc == nil || profileText == "" {
		return nil
	}

	hash := sha256Short(profileText)

	c.profileEmbeddingsMu.RLock()
	if entry, ok := c.profileEmbeddings[agentId]; ok && entry.textHash == hash && time.Since(entry.cachedAt) < profileEmbeddingTTL {
		c.profileEmbeddingsMu.RUnlock()
		return entry.embedding
	}
	c.profileEmbeddingsMu.RUnlock()

	vec, err := c.embedFunc(ctx, profileText, "")
	if err != nil || len(vec) == 0 {
		return nil
	}

	c.profileEmbeddingsMu.Lock()
	c.profileEmbeddings[agentId] = profileEmbeddingEntry{embedding: vec, textHash: hash, cachedAt: time.Now()}
	c.profileEmbeddingsMu.Unlock()
	return vec
}

// sha256Short returns a stable content-addressed hash of s used to
// fingerprint cached profile text (in-memory only). The exact format
// is implementation-defined; only equality matters.
func sha256Short(s string) string {
	return string(id.New().FromString(s))
}

// buildAgentProfileText creates a combined text description of an agent's profile,
// domains, and personality for use as embedding input.
func buildAgentProfileText(agent *agentPayload) string {
	var parts []string
	if agent.Description != "" {
		parts = append(parts, agent.Description)
	}
	if agent.Capabilities != nil {
		if domains, ok := agent.Capabilities["domains"].([]any); ok {
			for _, d := range domains {
				if s, ok := d.(string); ok {
					parts = append(parts, s)
				}
			}
		}
	}
	if agent.Personality != "" {
		parts = append(parts, agent.Personality)
	}
	return strings.Join(parts, ". ")
}

// NewCognitionIntegration creates a new Cognition integration instance.
func NewCognitionIntegration(baseIntegration *integrations.Integration, args ...CognitionArg) (*CognitionIntegration, error) {
	if baseIntegration == nil {
		var err error
		baseIntegration, err = integrations.NewIntegration(ComponentName)
		if err != nil {
			return nil, fmt.Errorf("create base integration: %w", err)
		}
	}

	cfg := defaultCognitionConfig()

	for _, arg := range args {
		if arg == nil {
			continue
		}
		arg.Apply(cfg)
	}

	// Validate required dependencies
	if cfg.engine == nil {
		return nil, fmt.Errorf("%w: engine is required", integrations.ErrInvalidConfiguration)
	}
	if cfg.providerRegistry == nil {
		return nil, fmt.Errorf("%w: provider registry is required", integrations.ErrInvalidConfiguration)
	}
	if cfg.eventBus == nil {
		return nil, fmt.Errorf("%w: event bus is required", integrations.ErrInvalidConfiguration)
	}
	if cfg.scoreEngine == nil {
		return nil, fmt.Errorf("%w: score engine is required", integrations.ErrInvalidConfiguration)
	}

	c := &CognitionIntegration{
		Integration:          baseIntegration,
		variableResolver:     cfg.variableResolver,
		engine:               cfg.engine,
		providerRegistry:     cfg.providerRegistry,
		eventBus:             cfg.eventBus,
		config:               cfg,
		scoreEngine:          cfg.scoreEngine,
		participantsCache:    make(map[string]cachedParticipants),
		spaceContextCache:    make(map[string]cachedSpaceContext),
		siParticipantCache:   make(map[string]cachedSIParticipant),
		agentCache:           make(map[string]cachedAgent),
		spaceInfoCache:       make(map[string]cachedSpaceInfo),
		recentUtterCache:     make(map[string][]map[string]any),
		attachmentsCache:     make(map[string]cachedAttachments),
		profileEmbeddings:    make(map[string]profileEmbeddingEntry),
		latestHumanUtterance: make(map[string]string),
		reactiveDispatchAt:   make(map[string]time.Time),
		greetingPacing:       make(map[string]*greetingPacingState),
		conductors:           NewConductorRegistry(),
	}

	// Update logger to use Cognition component name
	c.Logger = logger.New(ComponentName, os.Stdout, resolveLoggerLevel())

	// Dispatch gate: exactly-one-replica turn dispatch via a Postgres advisory
	// lock keyed by utterance id (znasllc-io/memql#1217). dbGetter is optional
	// -- a nil getter makes the gate fail safe (always proceeds), so
	// single-binary / no-DB builds are unaffected. See dispatch_gate.go.
	c.dispatchGate = newDispatchGate(cfg.dbGetter, c.Logger)

	// Phase 2 of llm-driven-decisions: spin up the message
	// classifier so the affirmation/follow-up suppression guard
	// can ask the LLM (cached) instead of grep'ing for magic
	// phrases. See message_classifier.go and
	// docs/internal/planning/llm-driven-decisions.md.
	c.classifier = newMessageClassifier(c.Logger, c.engine)

	c.Logger.Info("cognition integration created")

	return c, nil
}

// ComponentName returns the component identifier.
func (c *CognitionIntegration) ComponentName() common.ComponentName {
	return ComponentName
}

// Start begins the integration lifecycle and subscribes to events.
func (c *CognitionIntegration) Start(ctx context.Context) {
	if c == nil {
		return
	}

	c.Logger.Info("starting cognition integration")

	// Subscribe to space creation events to bootstrap space context.
	c.unsubscribes = append(c.unsubscribes, c.eventBus.Subscribe(
		eventPatternSpaceCreated,
		c.handleSpaceCreated,
		events.WithSubscriberName("cognition:space-context-engine"),
	))

	// Subscribe to participant changes to keep space context up to date.
	c.unsubscribes = append(c.unsubscribes, c.eventBus.Subscribe(
		eventPatternParticipantAdded,
		c.handleParticipantChanged,
		events.WithSubscriberName("cognition:space-context-engine"),
	))

	// Subscribe a SECOND handler on the same event for the
	// greet-on-join path. Lives in greet_on_join.go: when an SI
	// participant joins and its agent template carries
	// triggerBehavior.greetOnJoin == true (and no greeting has been
	// posted for this (space, agent) yet), the handler fires off an
	// LLM-driven brief greeting via the existing agent reply path.
	// Kept distinct from the space-context recompute handler so the
	// two concerns can evolve independently and so a stuck LLM call
	// can never starve the context recompute.
	c.unsubscribes = append(c.unsubscribes, c.eventBus.Subscribe(
		eventPatternParticipantAdded,
		c.handleSIParticipantGreeting,
		events.WithSubscriberName("cognition:greet-on-join"),
	))

	// Subscribe to session changes (camera/mic/avatar toggles) to keep space context up to date.
	c.unsubscribes = append(c.unsubscribes, c.eventBus.Subscribe(
		eventPatternSessionChanged,
		c.handleSessionChanged,
		events.WithSubscriberName("cognition:space-context-engine"),
	))

	// Subscribe to utterance creation events — Cognition handles turn-taking decisions
	// and SI response generation in a single pipeline (no intermediate turn:state record).
	c.unsubscribes = append(c.unsubscribes, c.eventBus.Subscribe(
		eventPatternUtteranceCreated,
		c.handleUtteranceForCognition,
		events.WithSubscriberName("cognition:cognition"),
	))
	c.Logger.Info("subscribed to utterance events for cognition",
		"pattern", eventPatternUtteranceCreated,
	)

	// Subscribe to client-tool-response events so cross-node client tool
	// calls (agent -> browser round-trip) can complete. See
	// client_tool_relay.go for the full flow.
	c.startClientToolRelay()

	// (Plan-execution dispatch lives in integrations/planner/, not
	// here -- cognition handles routing + turn-taking for chat
	// utterances; Plan / Task lifecycle and the AgentForwarder
	// dispatch for "Allow on a permission card" rides on the
	// planner node. The planner integration subscribes to
	// graph.node.updated.v1:planner:plan in its own Start.)

	// Start space context heartbeat for reconciliation.
	c.startSpaceContextHeartbeat(ctx)

	// Call base Start
	c.Integration.Start(ctx)
}

// Stop halts the integration lifecycle and unsubscribes from events.
func (c *CognitionIntegration) Stop(ctx context.Context) {
	if c == nil {
		return
	}

	c.Logger.Info("stopping cognition integration")

	c.stopSpaceContextHeartbeat()

	// Unsubscribe from events
	for _, unsub := range c.unsubscribes {
		if unsub != nil {
			unsub()
		}
	}
	c.unsubscribes = nil

	// Call base Stop
	c.Integration.Stop(ctx)
}

// PrewarmSpaceContext fires the per-space cache loaders for spaceId
// without blocking on the result. Called by the bridge agent the
// moment the user starts producing speech-classified RTP packets, so
// the participant / utterance / space-info / attachment caches are
// hot by the time the final transcript lands and the actual handler
// runs. Saves ~50-150ms off handlerOffsetMs on warm voice turns.
//
// Idempotent + best-effort -- if a loader fails we just log and
// move on. The caller (HTTP handler in polyphonws) returns 202
// Accepted immediately; the warmers run in background goroutines.
func (c *CognitionIntegration) PrewarmSpaceContext(spaceId string) {
	if c == nil || strings.TrimSpace(spaceId) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(4)
		go func() {
			defer wg.Done()
			_, _ = c.getParticipantsForPromptCached(ctx, spaceId)
		}()
		go func() {
			defer wg.Done()
			limit := c.ConversationHistoryLimit(ctx)
			_, _ = c.getRecentUtterancesForPromptCached(ctx, spaceId, clampInt(limit, 10, 30))
		}()
		go func() {
			defer wg.Done()
			_ = c.getSpaceInfoCached(ctx, spaceId)
		}()
		go func() {
			defer wg.Done()
			_ = c.getAttachmentsForPromptCached(ctx, spaceId)
		}()
		wg.Wait()
	}()
}

// ConversationHistoryLimit returns the configured limit for conversation history.
// Fetched from v1:platform:partitionVariable at runtime.
func (c *CognitionIntegration) ConversationHistoryLimit(ctx context.Context) int {
	const defaultLimit = 20

	if c == nil || c.variableResolver == nil {
		return defaultLimit
	}

	value, err := c.variableResolver(ctx, VarCognitionConversationHistory)
	if err != nil {
		return defaultLimit
	}

	// Parse integer from string
	var limit int
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &limit); err != nil || limit <= 0 {
		return defaultLimit
	}

	return limit
}

// DefaultProvider returns the configured default SI provider name.
// Fetched from v1:platform:partitionVariable at runtime.
func (c *CognitionIntegration) DefaultProvider(ctx context.Context) string {
	const defaultProvider = "chat54Mini"

	if c == nil || c.variableResolver == nil {
		return defaultProvider
	}

	value, err := c.variableResolver(ctx, VarCognitionDefaultProvider)
	if err != nil || strings.TrimSpace(value) == "" {
		return defaultProvider
	}

	return strings.TrimSpace(value)
}

// IsSIEnabled checks if SI responses are enabled.
// Fetched from v1:platform:partitionVariable at runtime.
func (c *CognitionIntegration) IsSIEnabled(ctx context.Context) bool {
	if c == nil || c.variableResolver == nil {
		return true // Default to enabled if no resolver
	}

	value, err := c.variableResolver(ctx, VarCognitionSIEnabled)
	if err != nil {
		return true // Default to enabled on error
	}

	return strings.ToLower(strings.TrimSpace(value)) != "false"
}
