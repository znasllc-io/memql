package memql

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/core/common"
)

// PluginContractVersion is the major version of the Plugin SDK -- the stable
// extension surface a pack compiles against: the PluginContext fields, the
// PluginFactory signature, and the registration primitives (RegisterPlugin
// here, dsl.RegisterTree, node.RegisterRoutingRule,
// server.RegisterReadinessCheck).
//
// Bump this ONLY on a BREAKING change to that surface (a removed/renamed
// PluginContext field, a changed PluginFactory signature, a changed
// registration primitive). Additive, backward-compatible changes (a new
// PluginContext field, a new optional primitive) do NOT bump it -- existing
// packs keep compiling and loading unchanged.
//
// A pack records the version it was built against via
// RegisterPluginForContract (or implicitly, the current version, via
// RegisterPlugin). The loader rejects a pack whose declared version is
// incompatible with this constant (see CheckPluginContractCompat) so a stale
// pack fails loudly at startup instead of silently mis-binding against a
// contract it was not built for. The canonical reference for the surface is
// docs/public/build/plugin-sdk.md.
const PluginContractVersion = 1

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
	//
	// This is the MAIN, transaction-POOLED endpoint -- use it for all bulk
	// queries and mutations.
	BunDB func() *bun.DB

	// DirectBunDB returns the handle for the DIRECT (non-pooled) endpoint,
	// falling back to the main pool when DIRECT_DSN is unset. Plug-ins that
	// hold a SESSION across statements -- session-scoped advisory locks,
	// leader election -- must resolve their *bun.DB through this getter so
	// they bypass the transaction pooler, which recycles a server backend
	// between statements and would drop a held session-scoped lock
	// (epic memql#1925). Bulk traffic must NOT use this getter.
	DirectBunDB func() *bun.DB

	// VisionProvider returns the default vision-capable SI provider, or nil.
	VisionProvider func() common.VisionAIProvider

	// EmbeddingProviderByName returns a named embedding provider, or an
	// error if no provider by that name is registered.
	EmbeddingProviderByName func(name string) (EmbeddingAIProvider, error)

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

	// RequiresContractVersion is the PluginContractVersion the pack was built
	// against. RegisterPlugin stamps the current version; a pack that pins an
	// explicit version uses RegisterPluginForContract. The loader validates it
	// against the core's PluginContractVersion before materializing the pack
	// (see ValidateContract / CheckPluginContractCompat).
	RequiresContractVersion int
}

// ValidateContract checks this registration's declared contract version
// against the core's PluginContractVersion. Returns a descriptive error,
// naming the plug-in, when the versions are incompatible.
func (r PluginRegistration) ValidateContract() error {
	if err := CheckPluginContractCompat(r.RequiresContractVersion); err != nil {
		return fmt.Errorf("plugin %q: %w", r.Name, err)
	}
	return nil
}

// CheckPluginContractCompat reports whether a pack declaring
// requiresContractVersion is compatible with the core's PluginContractVersion.
//
// Compatibility is exact-major equality: the SDK surface carries a single major
// version, additive changes keep it, and any mismatch is incompatible -- a pack
// built against a retired older contract (the core may have removed surface it
// relies on) OR one that needs a newer core than this binary provides (the core
// lacks surface it relies on). Returns a descriptive error on mismatch (or on a
// missing/undeclared version), nil when compatible.
func CheckPluginContractCompat(requiresContractVersion int) error {
	if requiresContractVersion <= 0 {
		return fmt.Errorf(
			"plug-in declares no contract version (got %d); register it via RegisterPlugin or RegisterPluginForContract so it is stamped against memql.PluginContractVersion=%d",
			requiresContractVersion, PluginContractVersion)
	}
	if requiresContractVersion != PluginContractVersion {
		return fmt.Errorf(
			"plug-in contract version mismatch: pack built against contract v%d, this core provides v%d -- rebuild the pack against this core",
			requiresContractVersion, PluginContractVersion)
	}
	return nil
}

var (
	pluginMu       sync.Mutex
	pluginRegistry []PluginRegistration
)

// RegisterPlugin registers an integration plug-in factory against the CURRENT
// PluginContractVersion. Called from an init() function in each plug-in
// package; build tags on the enclosing .go file control which node-type
// binaries include the registration.
//
// Panics on empty name, nil factory, or duplicate name -- these are
// programmer errors and should surface loudly at startup.
func RegisterPlugin(name string, factory PluginFactory) {
	RegisterPluginForContract(name, PluginContractVersion, factory)
}

// RegisterPluginForContract is RegisterPlugin with an explicit declared
// contract version -- the PluginContractVersion the pack was built against.
// Third-party packs SHOULD use this so their contract expectation is recorded
// explicitly; the loader rejects the pack at startup if that version is
// incompatible with the core's PluginContractVersion (see ValidateContract).
//
// Panics on empty name, nil factory, or duplicate name.
func RegisterPluginForContract(name string, requiresContractVersion int, factory PluginFactory) {
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
	pluginRegistry = append(pluginRegistry, PluginRegistration{
		Name:                    name,
		Factory:                 factory,
		RequiresContractVersion: requiresContractVersion,
	})
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
