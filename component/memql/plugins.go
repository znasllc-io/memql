package memql

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
)

// PluginContractVersion is the major version of the Plugin SDK -- the stable
// extension surface a pack compiles against: the PluginContext fields, the
// PluginFactory signature, and the registration primitives (RegisterPlugin
// here, dsl.RegisterTree, node.RegisterRoutingRule,
// server.RegisterReadinessCheck, knowledge.RegisterSeedDomain,
// RegisterAppProfile + RegisterCapabilitySlug here,
// node.RegisterChatReplyConcept).
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
//
// It is 2 as of epic memql#5127: PluginContext's VisionProvider and
// EmbeddingProviderByName were RENAMED to ResolveVisionProvider and
// ResolveEmbeddingProvider, and the three streaming-provider lookups were
// removed from IntegrationEngineAccess. Both are breaking changes to this
// surface by the rule above, so the version says so -- even though the
// compiler catches a stale pack first, since the Go half of a pack is
// compiled in rather than loaded.
const PluginContractVersion = 2

// PluginContext is the narrow Go surface every registrant -- a pack, or a
// core integration self-registering -- receives at startup through the
// registration primitives (RegisterPlugin / RegisterPluginForContract). It is
// the stable extension contract: a pack either finds what it needs here or on
// Engine. Reaching into app/ internals is not permitted.
//
// Vocabulary: docs/public/concepts/component-integration-pack.md. The
// registrant is a pack (or a core integration self-registering); "plugin"
// survives only inside the primitive names (RegisterPlugin, PluginContext,
// the Plugin SDK).
//
// PluginContext is built once by the app bootstrap after core state (engine,
// database, providers) is ready, then passed to every registered factory.
// Callbacks (BunDB, the resolvers) are lazily evaluated so packs see the live
// state even if they stash the context.
type PluginContext struct {
	Logger *slog.Logger

	// Engine provides DSL execution, AI invocation and tool dispatch. Use
	// this for anything that speaks to the MemQL engine surface. It carries
	// no provider lookup: a model is reached through the router, and the two
	// resolvers below are the only provider-shaped things on this context.
	Engine IntegrationEngineAccess

	// BunDB returns the database handle, or nil if the node-type binary
	// runs without a database. Packs that need DB access should return
	// an error from their factory when nil.
	//
	// This is the MAIN, transaction-POOLED endpoint -- use it for all bulk
	// queries and mutations.
	BunDB func() *bun.DB

	// DirectBunDB returns the handle for the DIRECT (non-pooled) endpoint,
	// falling back to the main pool when DIRECT_DSN is unset. Packs that
	// hold a SESSION across statements -- session-scoped advisory locks,
	// leader election -- must resolve their *bun.DB through this getter so
	// they bypass the transaction pooler, which recycles a server backend
	// between statements and would drop a held session-scoped lock
	// (epic memql#1925). Bulk traffic must NOT use this getter.
	DirectBunDB func() *bun.DB

	// GateDB returns the pool WRITE GATES run on (AcquireWriteGate), or nil on a
	// node without a database. It is a pool of its own -- connections used for
	// nothing else, whose sessions may sit idle in a transaction for minutes --
	// and a gate must be taken on it, never on the pool BunDB() hands out: the
	// work under a gate needs a connection of that pool, and its sessions end an
	// idle transaction after 60 s, taking the gate's lock with them (memql#6022).
	// Use it for gates and for nothing else. A plug-in that takes gates REFUSES a
	// context without it, at construction: the alternative is a capability that
	// works until its first decision and then answers that coordination is
	// unavailable. A test or a tool that builds such a plug-in gives it a pool
	// from database.NewGatePool.
	GateDB func() *sql.DB

	// ResolveVisionProvider RESOLVES a vision-capable provider through the
	// router, or returns nil when no door to one is open.
	//
	// THE NAME IS THE CONTRACT (epic memql#5127, design D2). It was
	// `VisionProvider`, which is the name of the registry accessor it used to
	// forward to, and a pack reading that field had every reason to believe
	// it was getting a lookup. It is a resolution: a level, a modality, a
	// rule, a decision record. Renaming it is what stops the two from being
	// confusable at the only place a pack sees either.
	ResolveVisionProvider func() common.VisionAIProvider

	// ResolveEmbeddingProvider resolves an embedding provider through the
	// router. A non-empty name is a PIN -- it rides
	// airoute.ResolveRequest.ExplicitProvider and wins over every rule --
	// rather than a registry lookup, so a binding that names its embedding
	// model still gets that model, and still passes through the doors, the
	// ceiling and the ledger.
	//
	// It takes the CALLER'S CONTEXT because a `fleet:<modelId>` name
	// resolves against the acting user's own machines (epic memql#5096):
	// resolved without one it would answer against the shared-inference
	// set, which reports a live laptop as absent.
	ResolveEmbeddingProvider func(ctx context.Context, name string) (EmbeddingAIProvider, error)

	// ResolvePartitionFromContext returns the active partition for the
	// given request context; "default" if none is set.
	ResolvePartitionFromContext func(ctx context.Context) string

	// ResolveVariable resolves a named partition-scoped plaintext
	// variable from v1:platform:partitionVariable, falling back to v1:platform:globalVariable
	// when the partition lookup misses. Packs that need an
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

	// Providers is the AI provider registry. Packs that catalog
	// providers (e.g. the router admin integration listing available
	// models) read from it directly. Lives on the engine -- pointer is
	// stable for the life of the process.
	Providers *ProviderRegistry

	// Policies is the AI Router policy registry loaded from
	// policies/v1/*.memql. Same stability contract as Providers.
	Policies *PolicyRegistry

	// ConceptDataIsStaged reports whether a concept's ROWS are STAGED --
	// present, addressable, open to writes, and withheld from the ordinary read
	// path until the concept is trained (epic memql#3974).
	//
	// Every pack that issues a HAND-ROLLED read against "MemoryNodes" must
	// consult this before returning rows. The DSL seams get the same fact from
	// the engine directly (memql#3983); a pack's raw SQL passes through
	// neither the parser nor the filter path, so nothing is injected into it and
	// this callback is the whole of the enforcement there.
	//
	// Wired from (*MemQLEngine).ConceptDataIsStaged, which is a one-line export
	// of the tier's own lookup -- one staged set, one door. A factory that needs
	// it must REFUSE to construct when it is nil rather than treating nil as
	// "nothing is staged": that default reads as working and publishes rows the
	// tier exists to withhold.
	//
	// Additive to the Plugin SDK surface, so PluginContractVersion does not move.
	ConceptDataIsStaged func(conceptId string) bool

	// AdmitSourceRow reports whether one row read DIRECTLY from the node store
	// may be shown to this caller -- the per-row authorization gate, applied to
	// the rows a pack fetched itself (memql#4029).
	//
	// The sibling of ConceptDataIsStaged above, answering the OTHER question
	// about the same row: that one asks "is this concept's data visible to
	// anyone yet", this one asks "may THIS caller see this row". A hand-rolled
	// read needs both, because it passes through neither the parser nor the
	// filter path and nothing is injected into it.
	//
	// IT MUST BE APPLIED TO THE ROWS AS FETCHED, before they are folded,
	// summarized or repacked. Both of the engine's row-authz mechanisms resolve
	// the tier from a CONCEPT, so a capability that reads real rows and returns
	// a synthetic node stamped with its own concept defeats them by
	// construction: the gate is asked about the summary's made-up concept, finds
	// no tier declared, and admits. Fixing that after the repack is not possible
	// -- the summary no longer carries the per-row payload an `owned` tier reads
	// or the per-row identity a `granted` tier needs.
	//
	// Wired from memql.AdmitSourceRow, a one-line delegation to the same gate
	// the engine's own seams use -- one gate, one door. Fail-CLOSED: an
	// undecidable tier is denied, because a hand-rolled SELECT carries no
	// injected conjunct and no join for it to defer to. A factory that needs it
	// must REFUSE to construct when it is nil, for the same reason
	// ConceptDataIsStaged does.
	//
	// Additive to the Plugin SDK surface, so PluginContractVersion does not move.
	AdmitSourceRow func(ctx context.Context, node memorynodes.MemoryNode) bool

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

// PluginRegistration names a registered pack factory -- one entry in the
// registry the RegisterPlugin / RegisterPluginForContract primitives append
// to.
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
// naming the pack, when the versions are incompatible.
func (r PluginRegistration) ValidateContract() error {
	if err := CheckPluginContractCompat(r.RequiresContractVersion); err != nil {
		return fmt.Errorf("pack %q: %w", r.Name, err)
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
			"pack declares no contract version (got %d); register it via RegisterPlugin or RegisterPluginForContract so it is stamped against memql.PluginContractVersion=%d",
			requiresContractVersion, PluginContractVersion)
	}
	if requiresContractVersion != PluginContractVersion {
		return fmt.Errorf(
			"pack contract version mismatch: built against contract v%d, this core provides v%d -- rebuild the pack against this core",
			requiresContractVersion, PluginContractVersion)
	}
	return nil
}

var (
	pluginMu       sync.Mutex
	pluginRegistry []PluginRegistration
)

// RegisterPlugin registers a pack (or a core integration self-registering)
// against the CURRENT PluginContractVersion. Called from an init() function in
// the registrant's package; build tags on the enclosing .go file control which
// node-type binaries include the registration.
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
		panic("memql.RegisterPluginForContract: empty pack name")
	}
	if factory == nil {
		panic("memql.RegisterPluginForContract: nil factory for pack " + name)
	}

	pluginMu.Lock()
	defer pluginMu.Unlock()

	for _, r := range pluginRegistry {
		if r.Name == name {
			panic("memql.RegisterPluginForContract: pack " + name + " already registered")
		}
	}
	pluginRegistry = append(pluginRegistry, PluginRegistration{
		Name:                    name,
		Factory:                 factory,
		RequiresContractVersion: requiresContractVersion,
	})
}

// RegisteredPlugins returns every registration made at init time (packs and
// self-registering core integrations), in registration order. The app
// bootstrap iterates this list after the engine and core state are ready.
func RegisteredPlugins() []PluginRegistration {
	pluginMu.Lock()
	defer pluginMu.Unlock()

	out := make([]PluginRegistration, len(pluginRegistry))
	copy(out, pluginRegistry)
	return out
}
