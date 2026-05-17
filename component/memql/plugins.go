package memql

import (
	"context"
	"log/slog"
	"sync"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/core/common"
)

// PluginContext is the narrow Go surface that self-registering integration
// plug-ins receive at startup. It is the stable extension contract: a plug-in
// either finds what it needs here or on Engine. Reaching into app/ internals
// is not permitted.
//
// PluginContext is built once by the app bootstrap after core state (engine,
// database, providers) is ready, then passed to every registered plug-in
// factory. Callbacks (BunDB, VisionProvider, resolvers) are lazily evaluated
// so plug-ins see the live state even if they stash the context.
type PluginContext struct {
	Logger *slog.Logger

	// Engine provides DSL execution, SI invocation, tool dispatch, and
	// streaming provider lookups. Use this for anything that speaks to the
	// MemQL engine surface.
	Engine IntegrationEngineAccess

	// BunDB returns the database handle, or nil if the node-type binary
	// runs without a database. Plug-ins that need DB access should return
	// an error from their factory when nil.
	BunDB func() *bun.DB

	// VisionProvider returns the default vision-capable SI provider, or nil.
	VisionProvider func() common.VisionSIProvider

	// EmbeddingProviderByName returns a named embedding provider, or an
	// error if no provider by that name is registered.
	EmbeddingProviderByName func(name string) (EmbeddingSIProvider, error)

	// ResolvePartitionFromContext returns the active partition for the
	// given request context; "default" if none is set.
	ResolvePartitionFromContext func(ctx context.Context) string

	// ResolveVariable resolves a named partition-scoped plaintext
	// variable from v1:platform:partitionVariable, falling back to v1:platform:globalVariable
	// when the partition lookup misses. Plug-ins that need an
	// instance-wide value should call ResolveSystemVariable instead.
	ResolveVariable func(ctx context.Context, name string) (string, error)

	// ResolveSystemVariable resolves a global plaintext variable from
	// v1:platform:globalVariable. Use this for instance-wide config (provider
	// names, feature flags, model defaults).
	ResolveSystemVariable func(ctx context.Context, name string) (string, error)

	// ResolveSecret resolves a partition-scoped encrypted secret from
	// v1:platform:partitionSecret, falling back to v1:platform:globalSecret. Returns the
	// decrypted plaintext.
	ResolveSecret func(ctx context.Context, name string) (string, error)

	// ResolveSystemSecret resolves a global encrypted secret from
	// v1:platform:globalSecret. Use this for instance-wide credentials
	// (vendor API keys, OAuth client secrets, integration credentials).
	ResolveSystemSecret func(ctx context.Context, name string) (string, error)

	// Providers is the SI provider registry. Plug-ins that catalog
	// providers (e.g. the router admin integration listing available
	// models) read from it directly. Lives on the engine -- pointer is
	// stable for the life of the process.
	Providers *ProviderRegistry

	// Policies is the SI Router policy registry loaded from
	// policies/v1/*.memql. Same stability contract as Providers.
	Policies *PolicyRegistry

	// Agents is the registry of DSL-declared agents loaded from
	// dsl/agents/v1/*.memql by LoadUnifiedAgents. The agents
	// integration's `agent(name, args)` builtin handler reads this to
	// resolve a name to its compiled AgentDefinition (system prompt,
	// tool refs, knowledge bindings, provider config). Lives on the
	// engine; pointer is stable for the life of the process.
	Agents *AgentRegistry
}

// PluginFactory constructs an IntegrationProvider from a PluginContext.
// Returning an error aborts app startup with a fatal log.
type PluginFactory func(pctx PluginContext) (IntegrationProvider, error)

// PluginRegistration names a registered plug-in factory.
type PluginRegistration struct {
	Name    string
	Factory PluginFactory
}

var (
	pluginMu       sync.Mutex
	pluginRegistry []PluginRegistration
)

// RegisterPlugin registers an integration plug-in factory. Called from an
// init() function in each plug-in package; build tags on the enclosing .go
// file control which node-type binaries include the registration.
//
// Panics on empty name, nil factory, or duplicate name -- these are
// programmer errors and should surface loudly at startup.
func RegisterPlugin(name string, factory PluginFactory) {
	if name == "" {
		panic("memql.RegisterPlugin: empty plugin name")
	}
	if factory == nil {
		panic("memql.RegisterPlugin: nil factory for plugin " + name)
	}

	pluginMu.Lock()
	defer pluginMu.Unlock()

	for _, r := range pluginRegistry {
		if r.Name == name {
			panic("memql.RegisterPlugin: plugin " + name + " already registered")
		}
	}
	pluginRegistry = append(pluginRegistry, PluginRegistration{Name: name, Factory: factory})
}

// RegisteredPlugins returns all plug-ins registered at init time, in
// registration order. The app bootstrap iterates this list after the
// engine and core state are ready.
func RegisteredPlugins() []PluginRegistration {
	pluginMu.Lock()
	defer pluginMu.Unlock()

	out := make([]PluginRegistration, len(pluginRegistry))
	copy(out, pluginRegistry)
	return out
}
