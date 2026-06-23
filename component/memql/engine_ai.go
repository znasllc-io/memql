package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/common"
)

func (e *MemQLEngine) Integrations() *IntegrationRegistry {
	return e.integrations
}

// IntegrationByName returns the registered IntegrationProvider with
// the supplied name, or nil if no such provider was registered. Used
// by node-type-specific wiring (e.g. cluster_workbench.go on the
// agent / workbench builds) to grab a typed handle on a plug-in
// instance after the engine has materialized it.
func (e *MemQLEngine) IntegrationByName(name string) IntegrationProvider {
	if e == nil || e.integrations == nil {
		return nil
	}
	return e.integrations.Provider(name)
}

// RegisterIntegration registers an IntegrationProvider, making its capabilities
// callable as builtin functions from the MemQL DSL. This is safe to call after
// engine startup (integrations start at order 160, after the engine).
//
// Each capability is injected into the builtin executor handler map under a
// fully-qualified name: integration.<integrationName>.<capabilityName>.
// Corresponding .memql builtin definitions with matching @executor values
// make these capabilities discoverable and callable from the DSL.
func (e *MemQLEngine) RegisterIntegration(provider IntegrationProvider) error {
	if err := e.integrations.Register(provider); err != nil {
		return err
	}

	// Ensure the builtin executor handler map is initialized.
	if e.builtinExecutorHandlers == nil {
		if err := e.initBuiltinExecutorHandlers(); err != nil {
			return fmt.Errorf("init builtin handlers: %w", err)
		}
	}

	// Inject each capability handler into the dispatch map and record
	// its PreserveOrder flag so the dispatch path can stamp monotonic
	// timestamps when the handler returns a pre-ordered slice.
	if e.builtinPreserveOrder == nil {
		e.builtinPreserveOrder = make(map[string]bool)
	}
	for _, cap := range provider.Capabilities() {
		fqn := qualifiedCapabilityName(provider.IntegrationName(), cap.Name)
		e.builtinExecutorHandlers[fqn] = cap.Handler
		if cap.PreserveOrder {
			e.builtinPreserveOrder[fqn] = true
		}
	}

	return nil
}

// WireSemanticCache constructs and attaches the semantic (vector) AI-call
// cache (5.9) to the AI runtime. It is wired from app bootstrap once the
// embedding provider and database handle are live (both arrive after the
// engine is constructed). Passing a nil embedding provider or dbGetter leaves
// the runtime exact-hash-only (a clean degrade -- the primitive becomes a
// no-op and every AI call behaves exactly as pre-5.9).
//
// embeddingProviderName selects the embedding model (empty => "embedding3Small",
// the instance default that matches node_vectors' 1536-dim schema). The
// namespace enablement registry is loaded from defaults + env here, so an
// operator can enable a vetted namespace without a rebuild.
func (e *MemQLEngine) WireSemanticCache(embeddingProviderName string, dbGetter func() *sql.DB) {
	if e == nil || e.aiRuntime == nil {
		return
	}
	if strings.TrimSpace(embeddingProviderName) == "" {
		embeddingProviderName = "embedding3Small"
	}
	if e.providers == nil {
		return
	}
	provider, err := e.providers.EmbeddingProvider(embeddingProviderName)
	if err != nil || provider == nil {
		if e.Logger != nil {
			e.Logger.Info("semantic AI cache not wired: embedding provider unavailable",
				"provider", embeddingProviderName, "err", err)
		}
		return
	}

	embedder := newProviderEmbedder(provider)
	store := newPGSemanticStore(dbGetter)
	if embedder == nil || store == nil {
		return
	}

	namespaces := loadSemanticNamespacesFromEnv()
	ttl := e.aiRuntime.cacheTTL(nil)
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	cache := newSemanticAICache(embedder, store, e.Logger, ttl, namespaces)
	e.aiRuntime.SetSemanticCache(cache)

	enabledCount := 0
	for _, cfg := range namespaces {
		if cfg.Enabled {
			enabledCount++
		}
	}
	if e.Logger != nil {
		e.Logger.Info("semantic AI cache wired",
			"embeddingProvider", embeddingProviderName,
			"enabledNamespaces", enabledCount)
	}
}

// SemanticCacheStats returns the semantic cache telemetry snapshot, or the
// zero value when the semantic cache isn't wired.
func (e *MemQLEngine) SemanticCacheStats() SemanticCacheStats {
	if e == nil || e.aiRuntime == nil || e.aiRuntime.semantic == nil {
		return SemanticCacheStats{}
	}
	return e.aiRuntime.semantic.Stats()
}

// InvokeAI invokes an AI prompt template with the provided data.
// This is the public interface for automation steps to execute AI functions.
func (e *MemQLEngine) InvokeAI(ctx context.Context, templateId string, data map[string]any) (any, error) {
	if e.aiRuntime == nil {
		return nil, fmt.Errorf("AI runtime is not configured")
	}

	invocation := &AIInvocation{
		TemplateId: templateId,
	}
	// Honor a context-attached provider override (memql#838 model
	// tiering): callers that want to escalate / downshift the model for
	// THIS invocation wrap the ctx with WithProviderOverride(ctx, name).
	// Mirrors the tool-loop paths (ai_tool_loop.go) which already do
	// this; without it, InvokeAI was the one AI entry point that ignored
	// the override and always used the prompt's @defaultProvider.
	if override := ProviderOverrideFromContext(ctx); override != "" {
		invocation.ProviderOverride = &override
	}

	return e.aiRuntime.Invoke(ctx, invocation, data)
}

// InvokeAIStructured renders the named prompt with the given data and
// invokes the prompt's default chat provider with provider-enforced
// structured output. Returns the raw JSON response as a string -- the
// caller parses it into its typed result struct.
//
// Use this for "logic" prompts (routing, classification, prediction,
// suggestion) where the output is parsed as JSON and the caller wants
// provider-level guarantees that the shape is valid. For prose-reply
// prompts (agentReply), use InvokeAI instead.
//
// schemaName is a short identifier the provider may surface in errors
// and traces (e.g. "cognitionRouting"). schema must be a valid JSON
// Schema document describing the expected object.
//
// When the prompt's provider does not implement ChatStructuredProvider,
// falls back to the default chat provider with schema instructions
// injected into the system prompt (best-effort, no shape guarantee).
// The log line `structured chat fallback` fires so the caller can see
// which providers lack native support.
//
// Caches the structured result through the same AI cache used by
// ai() invocations. Cache key includes templateId + schema name +
// schema body + rendered prompt text + provider name, so callers
// with identical inputs collapse to a single LLM round-trip across
// the entire memQL instance (multiple frontends, multiple users,
// any background work). TTL follows the AI cache config (default
// 60s, ceiling 300s -- short enough that an agent or domain
// re-train invalidates within a minute, long enough to swallow
// click-dismiss-click-again sequences). Frontend callers that
// want longer-lived caching keep their own per-utterance memo.
func (e *MemQLEngine) InvokeAIStructured(
	ctx context.Context,
	templateId string,
	data map[string]any,
	schemaName string,
	schema json.RawMessage,
	strict bool,
) (string, error) {
	if e.aiRuntime == nil {
		return "", fmt.Errorf("AI runtime is not configured")
	}
	rendered, err := e.RenderPrompt(templateId, data)
	if err != nil {
		return "", fmt.Errorf("render prompt %q: %w", templateId, err)
	}

	messages := []common.ChatMessage{
		{Role: "system", Content: rendered},
	}

	spec := common.StructuredSchema{
		Name:        schemaName,
		Description: fmt.Sprintf("%s output", templateId),
		Schema:      schema,
		Strict:      strict,
	}

	// Cache lookup. The key folds in the schema (name + body) so two
	// callers asking for different schemas off the same template get
	// separate entries. The provider name is the FIRST resolution
	// attempt (the prompt's @defaultProvider); on rare misroutes a
	// fallback path runs and writes a new entry under the same key
	// -- harmless because subsequent calls with the same template +
	// schema land on the same fallback again, so the cache reflects
	// reality.
	providerName := e.promptDefaultProvider(templateId)
	var cacheKey string
	var cacheTTL time.Duration
	if e.aiRuntime.cache != nil {
		cacheTTL = e.aiRuntime.cacheTTL(nil)
		if cacheTTL > 0 {
			cacheInput := strings.TrimSpace(schemaName) + "|" + string(schema) + "|" + rendered
			cacheKey = buildAICacheKey(templateId, providerName, cacheInput)
			if cached, ok := e.aiRuntime.cache.get(cacheKey); ok {
				if s, isString := cached.(string); isString {
					return s, nil
				}
			}
		}
	}

	// Prefer the prompt's declared provider; fall back to the default
	// structured-capable provider; last resort is the default chat
	// provider with schema instructions in-prompt.
	//
	// Provider-lifecycle (#1081): a prompt whose @defaultProvider has
	// been @disabled (so it is absent from the registry) resolves to
	// nil here and falls through to the default cleanly. Emit a single
	// log line so the fallback is observable rather than silent.
	var result string
	structured := e.StructuredChatProviderByName(providerName)
	if structured == nil && providerName != "" && e.Logger != nil {
		e.Logger.Info("prompt @defaultProvider unavailable; falling back to default structured provider",
			"template", templateId, "requestedProvider", providerName)
	}
	if structured != nil {
		result, err = structured.CallChatStructured(ctx, messages, spec)
	} else if structured := e.StructuredChatProvider(); structured != nil {
		result, err = structured.CallChatStructured(ctx, messages, spec)
	} else {
		chat := e.DefaultChatProvider()
		if chat == nil {
			return "", fmt.Errorf("no chat provider available for structured invocation")
		}
		fallbackMessages := []common.ChatMessage{
			{
				Role: "system",
				Content: fmt.Sprintf(
					"%s\n\nReturn ONLY JSON that matches this schema. No markdown, no prose:\n%s",
					rendered, string(schema),
				),
			},
		}
		result, err = chat.CallChat(ctx, fallbackMessages)
	}
	if err != nil {
		return "", err
	}

	if cacheKey != "" {
		e.aiRuntime.cache.set(cacheKey, result, cacheTTL)
	}
	return result, nil
}

// promptDefaultProvider returns the @defaultProvider for a named
// prompt template, or empty string if none is configured.
func (e *MemQLEngine) promptDefaultProvider(templateId string) string {
	if e == nil {
		return ""
	}
	if e.prompts == nil {
		return ""
	}
	if p, ok := e.prompts.Get(templateId); ok && p != nil {
		return strings.TrimSpace(p.DefaultProvider)
	}
	return ""
}

// ResolveVariable fetches a plaintext variable value from the
// v1:platform:partitionVariable (partition-scoped) concept, falling back to
// v1:platform:globalVariable (global) when the partition lookup misses. This
// fallback lets a tenant override an instance-wide value while keeping
// the instance default available as the backstop.
//
// Used by functions and automations to resolve var("NAME") expressions.

func (e *MemQLEngine) ReloadAIProviders(ctx context.Context) (int, error) {
	if e == nil {
		return 0, fmt.Errorf("engine is nil")
	}
	registry, err := loadAIProviders(e.Logger)
	if err != nil {
		return 0, fmt.Errorf("reload AI providers: %w", err)
	}
	e.providers = registry
	if e.aiRuntime != nil {
		e.aiRuntime = newAIRuntime(e.Logger, e.prompts, registry, e.aiCacheConfig)
	}
	if e.Logger != nil {
		e.Logger.Info("AI providers reloaded after seed",
			"providerCount", registry.Count())
	}
	return registry.Count(), nil
}

// TTSProvider returns the default TTS provider from the registry.
// Resolves MEMQL_DEFAULT_TTS_PROVIDER from v1:platform:globalVariable (the
// global instance default), then falls back to the first available
// TTS provider.
func (e *MemQLEngine) TTSProvider() TTSAIProvider {
	if e.providers == nil {
		return nil
	}
	defaultName, _ := e.ResolveSystemVariable(context.Background(), VarDefaultTTSProvider)
	return e.providers.TTSProvider(defaultName)
}

// TTSProviderByName returns a specific TTS provider by name.
func (e *MemQLEngine) TTSProviderByName(name string) (TTSAIProvider, bool) {
	if e.providers == nil {
		return nil, false
	}
	return e.providers.TTSProviderByName(name)
}

// VisionProvider returns the first available vision-capable provider from the registry.
func (e *MemQLEngine) VisionProvider() common.VisionAIProvider {
	if e.providers == nil {
		return nil
	}
	return e.providers.VisionProvider("")
}

// StreamProvider returns the default Streaming provider from the registry.
// Resolves MEMQL_DEFAULT_STREAM_PROVIDER from v1:platform:globalVariable (the
// global instance default), then falls back to the first available
// Streaming provider.
func (e *MemQLEngine) StreamProvider() StreamingAIProvider {
	if e.providers == nil {
		return nil
	}
	defaultName, _ := e.ResolveSystemVariable(context.Background(), VarDefaultStreamProvider)
	return e.providers.StreamProvider(defaultName)
}

// ChatStreamProvider returns the default streaming chat provider.
// Returns nil if no streaming provider is available.
func (e *MemQLEngine) ChatStreamProvider() common.ChatStreamProvider {
	sp := e.StreamProvider()
	if sp == nil {
		return nil
	}
	if csp, ok := sp.(common.ChatStreamProvider); ok {
		return csp
	}
	return nil
}

// DefaultChatProvider returns the default non-streaming chat provider for synchronous
// AI calls (e.g., suggest endpoints). Resolves the optional MEMQL_DEFAULT_CHAT_PROVIDER
// from v1:platform:globalVariable (instance default), then falls back to the first
// available non-streaming chat provider. Returns nil if no suitable provider
// is available.
func (e *MemQLEngine) DefaultChatProvider() common.ChatAIProvider {
	if e.providers == nil {
		return nil
	}
	defaultName, _ := e.ResolveSystemVariable(context.Background(), VarDefaultChatProvider)
	return e.providers.ChatProvider(defaultName)
}

// StructuredChatProvider returns a provider that enforces a JSON schema
// on the model output. Used for routing, classification, prediction,
// suggestion -- all the "logic" prompts where the output is parsed as
// JSON and a schema violation is a bug rather than degraded content.
// Falls back to DefaultChatProvider's name.
func (e *MemQLEngine) StructuredChatProvider() common.ChatStructuredProvider {
	if e.providers == nil {
		return nil
	}
	defaultName, _ := e.ResolveSystemVariable(context.Background(), VarDefaultChatProvider)
	return e.providers.ChatStructuredProvider(defaultName)
}

// StructuredChatProviderByName returns the named provider iff it
// implements ChatStructuredProvider. Callers supply the same name
// they'd pass to ChatProvider; returns nil when the named provider
// doesn't support structured output (caller should fall back).
func (e *MemQLEngine) StructuredChatProviderByName(name string) common.ChatStructuredProvider {
	if e.providers == nil {
		return nil
	}
	return e.providers.ChatStructuredProviderByName(name)
}

// SuggestChatProvider returns a fast, lightweight chat provider optimized for
// suggestion endpoints (spaces, agents, groups). Prefers smaller models that
// generate structured JSON quickly. Falls back to DefaultChatProvider if no
// fast model is available.
func (e *MemQLEngine) SuggestChatProvider() common.ChatAIProvider {
	if e.providers == nil {
		return nil
	}
	return e.providers.SuggestChatProvider()
}

// ChatStreamProviderByName returns a named streaming chat provider.
// Returns nil if the named provider doesn't exist or doesn't support streaming chat.
func (e *MemQLEngine) ChatStreamProviderByName(name string) common.ChatStreamProvider {
	if e.providers == nil || name == "" {
		return nil
	}
	csp, _ := providerByName[common.ChatStreamProvider](e.providers, name, nil)
	return csp
}

// ChatStreamWithToolsProviderByName returns a named provider that supports
// streaming chat with tool calling, or nil if the provider doesn't support it.
func (e *MemQLEngine) ChatStreamWithToolsProviderByName(name string) common.ChatStreamWithToolsProvider {
	if e.providers == nil || name == "" {
		return nil
	}
	p, _ := providerByName[common.ChatStreamWithToolsProvider](e.providers, name, nil)
	return p
}

// ToolDefinitionsForNames returns tool definitions for the listed tool names.
func (e *MemQLEngine) ToolDefinitionsForNames(names []string) []common.ToolDefinition {
	return e.toolsForToolCallingFiltered(names)
}

// ExecuteToolByName looks up a tool by name and executes it, returning the
// JSON result string.
func (e *MemQLEngine) ExecuteToolByName(ctx context.Context, name string, args map[string]any) (string, error) {
	if e == nil || e.tools == nil {
		return "", fmt.Errorf("tools not configured")
	}
	tool, err := e.tools.Get(strings.TrimSpace(name))
	if err != nil {
		return "", fmt.Errorf("tool %q not found: %w", name, err)
	}
	result, err := e.ExecuteTool(ctx, tool, args)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(result)
	return string(b), nil
}

// RenderPrompt renders a prompt template with the given data and returns the
// rendered text. Used by integrations that need to construct prompts for
// streaming calls where the standard InvokeAI path isn't used.
func (e *MemQLEngine) RenderPrompt(templateId string, data map[string]any) (string, error) {
	if e == nil || e.aiRuntime == nil {
		return "", fmt.Errorf("AI runtime is not configured")
	}
	prompt, err := e.aiRuntime.resolvePrompt(templateId)
	if err != nil {
		return "", err
	}
	payload, err := normalizeAIData(data)
	if err != nil {
		return "", fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}
	if err := prompt.ValidateData(payload); err != nil {
		return "", fmt.Errorf("data for prompt %q invalid: %w", prompt.Name, err)
	}
	return prompt.Render(payload)
}

// ProviderEntry returns a provider entry by name from the registry.
// Used by integrations that need access to AI providers.
func (e *MemQLEngine) ProviderEntry(name string) (*ProviderConfigEntry, bool) {
	if e.providers == nil {
		return nil, false
	}
	return e.providers.Entry(name)
}

// DefaultProviderName returns the name of the default AI provider.
func (e *MemQLEngine) DefaultProviderName() string {
	if e.providers == nil {
		return ""
	}
	return e.providers.Default()
}

// Providers returns the provider registry for direct provider access.
// Used by integrations that need to resolve specific provider types (e.g., embedding).
func (e *MemQLEngine) Providers() *ProviderRegistry {
	return e.providers
}

// Policies returns the AI Router policy registry loaded from
// policies/v1/*.memql. Used by the Router to resolve a policy name
// to a primary provider + fallback chain, and by the /router/policies
// admin page to list available policies.
func (e *MemQLEngine) Policies() *PolicyRegistry {
	return e.policies
}

// SetConfigSnapshot stashes the bus-distributed ConfigSnapshot used
// to build ctx.config inside policy bodies. The engine accepts the
// snapshot as an opaque any to avoid a static dependency on the
// component/bus protobuf package (which would create an import
// cycle with the bus consumers downstream). app bootstrap calls
// this once with the config component's Snapshot().
