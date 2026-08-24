package sync

import "context"

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
// The receiver calls this AFTER its env-configured sources, so an
// operator who pinned a source in the environment keeps it: a connector
// silently taking over a pinned name would move which secret verifies a
// live sender, with nothing in the environment changed to say so.
func SourceFor(ctx context.Context, name string) (InboundSource, bool) {
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
			return src, true
		}
	}
	return InboundSource{}, false
}
