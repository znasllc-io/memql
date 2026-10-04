package sync

import (
	"context"
	"fmt"
)

// inbound_source.go -- the webhook source a MULTI-TENANT connector owns
// (memql#4391).
//
// # Why this is not a method on Connector
//
// Only a connector serving many tenants needs one. The inbound
// receiver's env tier is resolved once at boot from
// MEMQL_INBOUND_SOURCE_<NAME>_*, which works for a sender an operator
// configures with the deployment and cannot work for a connector whose
// tenants are added at runtime: one Shopify connector serves many
// stores, each with its own webhook secret, and the secret therefore
// lives on the row that describes the store.
//
// Putting InboundSource on the Connector interface would oblige every
// implementation to answer a question most have no opinion about, and a
// connector with one tenant would return the same env-configured policy
// the receiver already has. So it is an OPTIONAL interface the receiver
// type-asserts for -- the shape Go's own optional interfaces take.

// InboundSourceProvider is implemented by a connector that owns webhook
// sources of its own.
type InboundSourceProvider interface {
	// InboundSource resolves the verification policy for one source
	// name. Reporting false is normal and means "not mine" -- the
	// receiver then falls through to its env-configured sources, and
	// answers 404 if none matches.
	InboundSource(ctx context.Context, name string) (InboundSource, bool)
}

// InboundSource is one connector-owned source's verification policy.
type InboundSource struct {
	// Name is the source segment in the URL: "<connector>-<tenant>".
	Name string
	// Scheme, SignatureHeader, SignaturePrefix and DedupeHeader mirror
	// the receiver's own SourceConfig vocabulary. They are ENCODINGS,
	// not vendors -- component/inbound deliberately carries no vendor
	// table, and this does not add one.
	Scheme          string
	SignatureHeader string
	SignaturePrefix string
	DedupeHeader    string
	// ForwardHeaders names the non-secret delivery metadata the receiver stages.
	// Values are not authenticated merely because the body signature matched.
	ForwardHeaders []string
	// Secret is the RESOLVED shared key. It lives here for exactly as
	// long as the verification takes: never staged on a row, never
	// logged, never returned to a caller.
	Secret string
	// SecretRef names where the secret came from, so a diagnostic can
	// say which reference failed without printing the secret itself.
	SecretRef string
}

// SourceName composes the inbound source segment for one of a
// connector's tenants.
//
// ONE spelling, used by the subscription registrar that tells the far
// end where to deliver and by the receiver that resolves what arrived.
// Two copies of this rule would disagree, and the disagreement is a
// webhook that 404s with every manifest looking correct.
func SourceName(connector, tenant string) string { return connector + "-" + tenant }

// SourceFor asks every BOUND connector whether it owns a source name,
// and returns the first that says yes.
//
// The receiver calls this AFTER its env-configured sources, so a
// connector never silently takes over a name an operator pinned: that
// would move which secret verifies a live sender, with nothing in the
// environment changed to say so. A pinned name ConnectorForSource
// resolves is REFUSED by the receiver instead of verified by either
// secret (memql#5707 follow-up).
func SourceFor(ctx context.Context, name string) (InboundSource, bool) {
	src, _, ok := sourceOwner(ctx, name)
	return src, ok
}

// ConnectorForSource resolves an exact connector name or a source explicitly
// claimed by a bound connector. It never guesses ownership from a prefix.
//
// It is the dispatcher's routing rule AND the inbound receiver's collision
// rule: a name this resolves is never verified by an env-pinned secret,
// because the connector it routes to reads the row as signed by its own. One
// predicate, so the receiver cannot admit a row the dispatcher would then
// hand to a connector on a premise the receiver did not check.
func ConnectorForSource(ctx context.Context, name string) (Connector, bool) {
	if c, ok := Lookup(name); ok {
		return c, true
	}
	_, c, ok := sourceOwner(ctx, name)
	return c, ok
}

// InboundSourceClaimer is OPTIONAL beside InboundSourceProvider. It answers
// whether a source name is the connector's WITHOUT resolving a secret, and it
// says when it cannot tell.
//
// The inbound receiver's env-pin collision check needs both properties. It
// runs before any signature check, on a path any caller can reach, so a
// secret resolved and unsealed only to be discarded is wasted work there.
// And "could not read my tenants" must never read as "not mine": that answer
// admits an env pin on a tenant's name, which the dispatcher then routes to
// the connector once the read recovers.
type InboundSourceClaimer interface {
	// ClaimsInboundSource reports whether name is one of the connector's
	// sources, exactly as InboundSource would claim it, or an error when the
	// connector cannot tell.
	ClaimsInboundSource(ctx context.Context, name string) (bool, error)
}

// SourceClaimed is ConnectorForSource's answer for the receiver's collision
// check: whether a bound connector claims name, and an error when one could
// not say. A connector that implements only InboundSourceProvider is asked
// through it, so the two answers agree wherever neither errors.
func SourceClaimed(ctx context.Context, name string) (Connector, bool, error) {
	if c, ok := Lookup(name); ok {
		return c, true, nil
	}
	for _, connectorName := range BoundNames() {
		c, ok := Lookup(connectorName)
		if !ok {
			continue
		}
		if claimer, ok := c.(InboundSourceClaimer); ok {
			claimed, err := claimer.ClaimsInboundSource(ctx, name)
			if err != nil {
				return c, false, fmt.Errorf("sync: connector %q could not say whether it claims source %q: %w", connectorName, name, err)
			}
			if claimed {
				return c, true, nil
			}
			continue
		}
		if provider, ok := c.(InboundSourceProvider); ok {
			if _, ok := provider.InboundSource(ctx, name); ok {
				return c, true, nil
			}
		}
	}
	return nil, false, nil
}

func sourceOwner(ctx context.Context, name string) (InboundSource, Connector, bool) {
	for _, connectorName := range BoundNames() {
		c, ok := Lookup(connectorName)
		if !ok {
			continue
		}
		provider, ok := c.(InboundSourceProvider)
		if !ok {
			continue
		}
		if src, ok := provider.InboundSource(ctx, name); ok {
			return src, c, true
		}
	}
	return InboundSource{}, nil, false
}
