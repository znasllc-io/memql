// Package liveknowledge owns the runtime dispatch for v1:knowledge:liveSource
// queries (Phase 5 of the planner-redesign work). Live Knowledge is the
// volatile-data counterpart to trained knowledge: where a trained
// knowledgeDomain holds pre-embedded chunks that change rarely, a
// liveSource is a current-state read against a backing connector
// (inventory, employees, calendar, tickets, etc.).
//
// The package shape:
//
//   - Connector: the kind-specific adapter that knows how to talk to one
//     class of upstream (memql concept query, postgres SQL, REST API, etc.).
//   - Registry: kind -> Connector lookup. Built-in connectors register at
//     init() time; plug-ins via integrations/<name>/ can self-register.
//   - QueryFn / Dispatcher: takes (liveSourceName, args), resolves the
//     source row + its backing connector, dispatches, returns the result.
//     Optional snapshot caching gated by the source's cachePolicy.
//
// Phase 5 scope: the runtime is scaffolded with the memql-kind connector
// (queries against the local engine) only. Postgres / REST / GraphQL /
// custom connectors land as follow-ups -- each is a self-contained
// implementation of the Connector interface registered against the
// matching `kind` value.
//
// Wiring: an integration in integrations/liveknowledge/ exposes the
// integration.liveknowledge.query capability via the DSL plug-in system
// so prompt-context builders + the Planner Agent's between-Task hook
// can call it.
package liveknowledge

import (
	"context"
	"fmt"
	"sync"
)

// Connector is the kind-specific adapter for one class of upstream
// source. The Registry holds one Connector per `kind` value from the
// v1:knowledge:liveConnector concept's enum (memql / postgres / mysql /
// mssql / rest / graphql / custom).
type Connector interface {
	// Kind returns the connector kind string the Registry keys on.
	// Must match the corresponding v1:knowledge:liveConnector.kind enum
	// value exactly.
	Kind() string

	// Query dispatches a single read against the upstream. The args
	// map is the raw caller-supplied input (already validated against
	// the liveSource's argsSchema before reaching this point). The
	// returned map is the populated response payload; the dispatcher
	// is responsible for any subsequent shape adaptation.
	Query(ctx context.Context, src Source, args map[string]any) (map[string]any, error)
}

// Source is the parsed-out liveSource + liveConnector pair the
// dispatcher hands to a Connector. Connectors don't read from the
// engine themselves; the dispatcher loads both rows and passes the
// relevant fields here.
type Source struct {
	// Id is the v1:knowledge:liveSource.id.
	Id string

	// Name is the human + agent-readable identifier (e.g.
	// "inventory.skuLevels"). Used in error messages and logs.
	Name string

	// QueryTemplate is the connector-kind-specific query body. For
	// memql connector: a MemQL query string with {args.x} placeholders.
	// For postgres connector: SQL with $1/$2 positional params. For
	// rest connector: a URL + body template. The connector
	// implementation interprets this; the dispatcher doesn't parse it.
	QueryTemplate string

	// ConnectorKind is the resolved Connector.Kind() value.
	ConnectorKind string

	// ConnectorEndpoint is the connector-kind-specific endpoint --
	// hostname:port/db for SQL kinds, base URL for rest/graphql,
	// empty for the memql kind.
	ConnectorEndpoint string

	// AuthSecretName names the v1:platform:partitionSecret /
	// globalSecret row that carries the credential. Resolved at
	// query time by the connector implementation so credential
	// rotation doesn't require re-registering the source.
	AuthSecretName string

	// ResultSchema (jsonb on the wire; map[string]any in Go) is the
	// expected response shape. Connectors may validate against this;
	// the dispatcher doesn't enforce it -- callers consume the raw
	// result and shape-adapt for their needs.
	ResultSchema map[string]any
}

// Registry holds the kind -> Connector lookup. Thread-safe; connectors
// register at init() time via the plug-in path or at app/ wiring time
// for first-party kinds.
type Registry struct {
	mu         sync.RWMutex
	connectors map[string]Connector
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{connectors: map[string]Connector{}}
}

// Register installs a connector under its Kind() value. Re-registering
// the same kind overwrites the previous binding (useful in tests).
func (r *Registry) Register(c Connector) {
	if c == nil || c.Kind() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectors[c.Kind()] = c
}

// Lookup returns the connector for a kind, or (nil, false) when none
// is registered.
func (r *Registry) Lookup(kind string) (Connector, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.connectors[kind]
	return c, ok
}

// MustLookup is a convenience for dispatch sites that treat an
// unregistered kind as fatal.
func (r *Registry) MustLookup(kind string) (Connector, error) {
	c, ok := r.Lookup(kind)
	if !ok {
		return nil, fmt.Errorf("liveknowledge: no connector registered for kind %q", kind)
	}
	return c, nil
}

// DefaultRegistry is a package-level singleton for the common case.
// Plug-ins / app/ wiring can call DefaultRegistry().Register(...) to
// install built-in connectors at startup.
var defaultRegistry = NewRegistry()

// DefaultRegistry returns the package-level Registry. Use the singleton
// when your wiring doesn't need a separate registry instance per node.
func DefaultRegistry() *Registry { return defaultRegistry }
