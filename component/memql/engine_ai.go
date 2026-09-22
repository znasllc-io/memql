package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"

	"github.com/znasllc-io/memql/component/metrics"
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
// embeddingProviderName selects the embedding model. EMPTY NOW MEANS "ask the
// cluster's embedder binding" rather than "embedding3Small" (epic memql#5137,
// D6): that literal was one of five copies of the same paid pin, and it also
// encoded node_vectors' 1536-dim schema as a fact about the whole cluster. A
// cluster with no binding wires no semantic cache, which is the honest degrade
// -- the alternative is embedding into a space nobody chose. The namespace
// enablement registry is loaded from defaults + env here, so an operator can
// enable a vetted namespace without a rebuild.
func (e *MemQLEngine) WireSemanticCache(embeddingProviderName string, dbGetter func() *sql.DB) {
	if e == nil || e.aiRuntime == nil {
		return
	}
	if strings.TrimSpace(embeddingProviderName) == "" {
		bound, err := ResolveEmbedderProvider(context.Background())
		if err != nil {
			if e.Component != nil && e.Logger != nil {
				e.Logger.Info("semantic AI cache not wired: no embedder is bound",
					"component", ComponentName)
			}
			return
		}
		embeddingProviderName = bound
	}
	if e.providers == nil {
		return
	}
	provider, err := e.providers.EmbeddingProvider(context.Background(), embeddingProviderName)
	if err != nil || provider == nil {
		if e.Component != nil && e.Logger != nil {
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
	// Export the semantic cache's counters on /metrics (memql#4532); see
	// aiResponseCacheWithMetrics for why this is a snapshot read rather
	// than a second set of counters.
	metrics.RegisterAICacheStatsSource(metrics.AICacheSemantic, func() metrics.CacheStats {
		s := cache.Stats()
		return metrics.CacheStats{
			Hits:      uint64(max64(s.Hits, 0)),
			Misses:    uint64(max64(s.Misses, 0)),
			KeysAdded: uint64(max64(s.Sets, 0)),
		}
	})
	e.aiRuntime.SetSemanticCache(cache)

	enabledCount := 0
	for _, cfg := range namespaces {
		if cfg.Enabled {
			enabledCount++
		}
	}
	if e.Component != nil && e.Logger != nil {
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

	// ONE SEAM (epic memql#5127, design D2). The prompt declares a LEVEL and
	// this call derives modality `structured`; the router picks the provider
	// and records the decision.
	//
	// THREE RESOLUTION TIERS ARE DELETED HERE, and each was a way to reach a
	// model nobody chose:
	//
	//   1. `StructuredChatProviderByName(promptDefault)`, which was the pin --
	//      it survives, as ExplicitProvider on the request, still winning over
	//      every rule.
	//   2. A REGISTRY-WIDE SCAN for anything structured-capable. Right for a
	//      cloud provider that is momentarily unregistered and catastrophic for
	//      a fleet one: a closed laptop silently handed every structured turn
	//      to a paid API, and nothing about the answer said so. The chain does
	//      this properly now -- it tries the doors in cost order and refuses
	//      with a report naming each.
	//   3. The DEFAULT CHAT provider with the schema pasted into the user turn.
	//      That is not a structured call; it is a chat call wearing one, which
	//      is why it needed stripJSONFences to survive a model that fenced its
	//      answer. A chain that cannot serve `structured` now refuses, and the
	//      refusal names the doors.
	//
	// refuseUnavailableLocalProvider is gone with them: it existed because
	// tier 2 would otherwise have fallen through a shut local door, and there
	// is no tier 2 left to fall through.
	prompt, ok := e.prompts.Get(templateId)
	if !ok || prompt == nil {
		return "", fmt.Errorf("unknown prompt template %q", templateId)
	}
	req, err := requestForPrompt(ctx, prompt, nil, airoute.ModalityStructured, rendered)
	if err != nil {
		return "", err
	}
	call, err := e.CallAIStructured(ctx, req, messages, spec)
	if err != nil {
		return "", err
	}
	result := call.Text

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

// safeLogger returns this engine's logger, or nil when it has none.
//
// `MemQLEngine.Logger` is PROMOTED from the embedded *component.Component, so
// on an engine built without one -- every hand-constructed test engine, and
// any embedding that skips the component wiring -- reading the field
// dereferences a nil pointer instead of yielding a nil logger. That is the
// memql#2674 class, and it is why every guard in this package tests the
// Component before the promoted field;
// TestNoUnguardedPromotedLoggerChecks fails the build on one that does not.
//
// This is the same rule with one name instead of a repeated conjunction, for
// a path that needs the logger three times. It is not an exemption from that
// gate: the gate looks for a comparison against the promoted field, and this
// function does not make one -- it checks the EMBEDDED POINTER and then reads
// the field, which is the only order that is safe. Every slog call in the
// package accepts a nil *slog.Logger, so reading the field is the whole
// hazard.
func (e *MemQLEngine) safeLogger() *slog.Logger {
	if e == nil || e.Component == nil {
		return nil
	}
	return e.Logger
}

func (e *MemQLEngine) ReloadAIProviders(ctx context.Context) (int, error) {
	if e == nil {
		return 0, fmt.Errorf("engine is nil")
	}
	if e.providers == nil {
		return 0, fmt.Errorf("reload AI providers: no provider registry on this engine")
	}

	// BUILD FULLY, THEN SWAP (epic memql#4440, design D5).
	//
	// Everything expensive and everything fallible happens out here on a
	// throwaway registry: the tree is walked, every auth placeholder is
	// re-resolved through globalSecret -> globalVariable -> env, and every SDK
	// client is constructed. Only when that has completed does anything the
	// running engine can see change.
	//
	// TWO BUGS ARE BEING FIXED HERE, not one, and the second is why this could
	// be promoted to a production seam at all:
	//
	//  1. IT LOADED NOTHING. `loadAIProviders` returns an EMPTY registry -- it
	//     says so in its own doc comment; the legacy walk it used to do was
	//     retired when providers moved to LoadUnifiedProviders, and this call
	//     site was never updated. So every "reload" replaced the live registry
	//     with an empty one and reported a count of zero. It had no callers in
	//     the tree, which is exactly why that went unnoticed.
	//  2. IT WAS NOT ATOMIC. Reassigning `e.providers` races ~57 unsynchronized
	//     reads of that field, and rebuilding `e.aiRuntime` to match discards
	//     the semantic cache SetSemanticCache attached to the old one.
	//     `adoptContents` swaps the registry's CONTENTS under its own write
	//     lock instead, so every reader keeps the same pointer, every accessor
	//     already holds RLock, and an in-flight call sees the whole old set or
	//     the whole new one -- never a mixture. aiRuntime is left alone: it
	//     holds the same registry, so it sees the new contents without being
	//     rebuilt.
	// safeLogger, not e.Logger: `Logger` is promoted from the embedded
	// *component.Component, which is nil on a hand-built engine, so reading
	// the field directly panics rather than yielding nil. Every other call
	// site in this file guards with `e.Component != nil && e.Logger != nil`
	// for that reason; this reload has three uses of it, so it resolves once.
	logger := e.safeLogger()

	next := newProviderRegistry()
	if _, err := LoadUnifiedProviders(logger, next); err != nil {
		// The LIVE registry is untouched. A reload that cannot build a
		// replacement leaves the node serving what it was already serving,
		// which is strictly better than the empty set it used to install.
		return 0, fmt.Errorf("reload AI providers: %w", err)
	}
	// NO finalizeDefault HERE, deliberately, and the omission is the careful
	// part rather than a gap.
	//
	// Boot does not call it after LoadUnifiedProviders either -- `setEntry`
	// maintains the default incrementally as each provider registers
	// (@default wins, else first-available-wins, in file order), so the
	// registry leaves the load with the right answer already.
	//
	// Calling it would ALSO be actively wrong here: `finalizeDefault` does not
	// honour `defaultPinned`. An operator who set MEMQL_DEFAULT_CHAT_PROVIDER
	// to a provider that cannot currently resolve would have their pin
	// silently cleared and replaced by whatever the map's iteration order
	// happened to surface first -- non-deterministic, so two replicas
	// reloading the same configuration could end up with different defaults.
	// A reload must leave the registry in the state a fresh boot would.
	e.providers.adoptContents(next)

	available := e.providers.AvailableCount()
	if logger != nil {
		logger.Info("AI providers reloaded",
			"component", ComponentName,
			"registered", e.providers.Count(),
			// AVAILABLE, not registered, is the number an operator is asking
			// for: a keyless node registers every provider and can call none.
			"available", available)
	}
	return available, nil
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

// StreamProvider returns a streaming provider from the registry.
//
// IT NAMES NOTHING (epic memql#5137, D3). It used to resolve
// MEMQL_DEFAULT_STREAM_PROVIDER first, which let a deployment manifest pin
// every streaming call to one paid vendor model with nothing in the graph
// recording that it had. The empty name asks the registry for the first
// available streaming provider, which on a cluster with no federation is none
// -- and nil is the honest answer there, because the caller's alternative is
// a paid call the operator never asked for.
func (e *MemQLEngine) StreamProvider() StreamingAIProvider {
	if e.providers == nil {
		return nil
	}
	return e.providers.StreamProvider("")
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

// DefaultChatProvider returns a non-streaming chat provider for synchronous AI
// calls. Nil when none is available.
//
// IT NAMES NOTHING, for the reason on StreamProvider above:
// MEMQL_DEFAULT_CHAT_PROVIDER is deleted (epic memql#5137, D3).
func (e *MemQLEngine) DefaultChatProvider() common.ChatAIProvider {
	if e.providers == nil {
		return nil
	}
	return e.providers.ChatProvider("")
}

// StructuredChatProvider returns a provider that enforces a JSON schema
// on the model output. Used for routing, classification, prediction,
// suggestion -- all the "logic" prompts where the output is parsed as
// JSON and a schema violation is a bug rather than degraded content.
// Names nothing, for the reason on StreamProvider above.
func (e *MemQLEngine) StructuredChatProvider() common.ChatStructuredProvider {
	if e.providers == nil {
		return nil
	}
	return e.providers.ChatStructuredProvider("")
}

// StructuredChatProviderByName returns the named provider iff it
// implements ChatStructuredProvider. Callers supply the same name
// they'd pass to ChatProvider; returns nil when the named provider
// doesn't support structured output (caller should fall back).
func (e *MemQLEngine) StructuredChatProviderByName(ctx context.Context, name string) common.ChatStructuredProvider {
	if e.providers == nil {
		return nil
	}
	return e.providers.ChatStructuredProviderByName(ctx, name)
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

// ToolCallee answers how a call to the named tool is written as a statement:
// the kind and name of the construct its function handler calls (epic
// memql#5370). A tool is agent-only, so no statement calls one; a statement
// reaches what the tool ran through its handler. ok is false for a tool this
// engine does not have, and for a query or webhook handler: those substitute
// the tool's arguments into MemQL text or a request, so the tool's arguments
// are not the arguments of any one call.
func (e *MemQLEngine) ToolCallee(name string) (kind, callee string, ok bool) {
	if e == nil || e.tools == nil || e.functions == nil {
		return "", "", false
	}
	tool, err := e.tools.Get(strings.TrimSpace(name))
	if err != nil || tool == nil || tool.Handler == nil || tool.Handler.Type != "function" {
		return "", "", false
	}
	callee = strings.TrimSpace(tool.Handler.FunctionName)
	fn, err := e.functions.Get(callee)
	if err != nil || fn == nil {
		return "", "", false
	}
	switch {
	case fn.IsBuiltin():
		return "builtin", callee, true
	case fn.FunctionKind == "mutation", fn.FunctionKind == "logic":
		return fn.FunctionKind, callee, true
	case fn.FunctionKind == "", fn.FunctionKind == "query":
		return "query", callee, true
	}
	return "", "", false
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

// Rules returns the routing-rule registry -- the corpus a call's metadata is
// matched against, in the evaluation order the registry fixed at load.
//
// It is NIL until the rule loader is wired, and every caller must treat nil as
// "this engine has no rules", not as "no rules matched". The distinction is
// the whole difference between a router that refuses and one that silently
// falls back to a precedence nobody wrote.
func (e *MemQLEngine) Rules() *RuleRegistry {
	return e.rules
}

// SetConfigSnapshot stashes the bus-distributed ConfigSnapshot used
// to build ctx.config inside policy bodies. The engine accepts the
// snapshot as an opaque any to avoid a static dependency on the
// component/bus protobuf package (which would create an import
// cycle with the bus consumers downstream). app bootstrap calls
// this once with the config component's Snapshot().
