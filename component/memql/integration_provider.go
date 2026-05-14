package memql

// IntegrationCapability describes a single callable function exposed by an integration.
// Capabilities are registered with the engine and become callable as builtin functions
// from the MemQL DSL.
type IntegrationCapability struct {
	// Name is the short capability name (e.g., "scoreUtterance").
	// At registration time it is namespaced as "integration.<integrationName>.<name>"
	// for dispatch via the builtin executor handler map.
	Name string

	// Description is a human-readable summary of what this capability does.
	Description string

	// Handler is the Go function that executes the capability.
	// It reuses the builtin executor handler signature so capabilities slot
	// directly into the engine's existing dispatch mechanism.
	Handler builtinExecutorHandler

	// ArgsSchema is an advisory description of expected arguments.
	// Keys are argument names, values are type hints (e.g., "string", "object").
	// Used for documentation and introspection via the functions() builtin.
	ArgsSchema map[string]string

	// PreserveOrder tells the engine that the handler's returned node
	// slice is already ordered (e.g. similarity-ranked chunks from a
	// pgvector cosine search, recency-weighted hits from an external
	// store) and the caller should observe that same order without
	// having to re-sort client-side.
	//
	// Implementation: when true, the dispatch path stamps monotonically
	// decreasing CreatedAt timestamps on each returned node (handler's
	// 0th element gets the newest timestamp, index N gets N nanoseconds
	// earlier). The downstream nodesToMap + mapToSortedSlice fallback
	// sorts by CreatedAt DESC, so the synthetic timestamps reproduce
	// the handler's original slice order. A caller that applies an
	// explicit sort() directive still wins -- this flag only restores
	// what you'd expect in the common "handler already sorted it"
	// default-sort case.
	//
	// Leave false (the default) for handlers that don't care about
	// order. The only current consumer is knowledge.lookup.
	PreserveOrder bool
}

// IntegrationProvider is implemented by integrations to formally register
// callable capabilities with the MemQL engine.
//
// Integrations are protocol adapters: they bridge external services (OpenAI
// voice, Deepgram voice, etc.) into the memQL ecosystem. They emit events
// inward and expose capabilities outward. Orchestration logic (queries, AI
// invocations, mutations, decision trees) belongs in MemQL DSL automations
// and functions, NOT in integration Go code.
//
// The capabilities registered here become callable as builtin functions from
// the DSL, enabling automations to invoke protocol-level operations without
// the integration needing direct access to the MemQL engine.
type IntegrationProvider interface {
	// IntegrationName returns a stable, unique identifier for this integration
	// (e.g., "cognition", "openaiVoice").
	IntegrationName() string

	// Capabilities returns the list of capabilities this integration exposes.
	// Called once during registration. Return nil for integrations that have
	// no callable capabilities (pure event emitters / protocol adapters).
	Capabilities() []IntegrationCapability
}
