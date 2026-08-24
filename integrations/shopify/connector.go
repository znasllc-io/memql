package shopify

// The Shopify CONNECTOR (epic memql#4378, D8) -- the first implementer
// of the connector contract, and deliberately not a new capability.
//
// Everything here already existed: applyInboundProduct parsed a staged
// webhook and upserted the thin index, reconcileProduct/reconcileIndex
// re-fetched known GIDs, and a miss retired a row rather than inventing
// one. What the contract adds is a NAME. v1:shopify:shopifyProduct now
// declares @origin("shopify"), which makes it a mirror -- read-only to
// users, agents, tools and raw inserts alike -- and this connector is
// the one writer the engine admits, under auth.ConnectorActor("shopify").
//
// That is the whole point of proving the contract here first: the
// behaviour is unchanged and testable against what shipped, so a
// difference in the rows or the audit trail is a defect in the contract
// rather than a change of product intent.
//
// Backfill, Propagate and EnsureSubscriptions are NOT implemented. They
// return the contract's typed not-implemented error, which the runtime
// distinguishes from a delivery failure -- so an outbox entry aimed at
// Shopify reports an unconfigured capability instead of dead-lettering.
// J1 fills them when the connector widens past products.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// ConnectorName is the name concepts write in @origin, and the name the
// registry knows this connector by. Spelled once.
const ConnectorName = "shopify"

// ProductConcept is the canonical id of the mirrored product concept.
const ProductConcept = "v1:shopify:shopifyProduct"

// init declares the connector NAME.
//
// Declaration is separate from binding, and the split is what makes the
// engine's boot check possible: Init runs before integrations are wired
// (app/build_*.go: config -> database -> engine -> integrations), so a
// registry holding only live instances would be empty exactly when the
// check reads it and would refuse every @origin in the tree. An init()
// runs before all of that.
func init() {
	memqlsync.Declare(ConnectorName)
}

// connector adapts the existing Integration to the contract. It holds
// the Integration rather than duplicating it, so there is exactly one
// implementation of "apply a Shopify product" and the contract is a
// surface over it rather than a second copy.
type connector struct{ i *Integration }

// Connector returns the contract surface over this integration.
func (i *Integration) Connector() memqlsync.Connector { return connector{i: i} }

func (c connector) Name() string { return ConnectorName }

func (c connector) Domains() []memqlsync.DomainSpec {
	return []memqlsync.DomainSpec{{
		Concept: ProductConcept,
		// Shopify's product payloads carry updated_at, but the thin
		// index does not STORE it -- three fields only (memql#4137).
		// Leaving VersionField empty tells the runtime to compare
		// against the version the connector stamps on each MirrorWrite,
		// which for this domain is the delivery time. That is the
		// fallback D6 names for an origin that offers nothing better,
		// and it is honest about what this index actually keeps.
		VersionField: "",
		// Six hours, matching the reconcileShopifyIndex schedule this
		// domain has had since memql#4137. The value lives here now so
		// the runtime can schedule it from the domain rather than from
		// a hand-written automation per connector.
		ReconcileInterval: 6 * time.Hour,
		Direction:         memqlsync.DirectionInbound,
	}}
}

// Apply turns one staged inbound delivery into mirror writes.
//
// It returns NO writes and no error for a delivery this connector does
// not recognise, and for an unconfigured Shopify. Both are normal: the
// inbound dispatcher offers every staged row to the connector its source
// names, and "not a product delivery" is the common case rather than a
// failure.
//
// The write itself still goes through the existing commit path -- the
// engine mutations upsertShopifyProduct / retireShopifyProduct under the
// connector actor -- because those carry the concept's field contract.
// The returned MirrorWrite describes what was written, which is what the
// runtime records and audits.
func (c connector) Apply(ctx context.Context, req memqlsync.InboundRequest) ([]memqlsync.MirrorWrite, error) {
	if c.i == nil || c.i.client == nil {
		return nil, nil
	}
	gid, handle, ok := ParseProductDelivery(req.Source, req.Topic, req.Body)
	if !ok {
		return nil, nil
	}
	version := req.ReceivedAt
	if version.IsZero() {
		version = time.Now().UTC()
	}
	return c.i.applyFetchedAsMirrorWrites(ctx, gid, handle, version)
}

// Reconcile re-fetches this domain's present rows: a hit upserts, a miss
// retires, and nothing new is invented.
//
// `since` is accepted and not used. Shopify's thin index keeps no
// per-row timestamp to compare against (three fields, memql#4137), so
// there is no way to ask "what changed since" without adding a field
// and a Shopify query this slice does not have. Sweeping everything
// present is what shipped and what this preserves; a narrower sweep is
// J1's, once the index carries updated_at.
func (c connector) Reconcile(ctx context.Context, conceptName string, _ time.Time) (memqlsync.ReconcileReport, error) {
	if c.i == nil || c.i.client == nil {
		return memqlsync.ReconcileReport{}, nil
	}
	if conceptName != "" && conceptName != ProductConcept {
		return memqlsync.ReconcileReport{}, memqlsync.NotImplemented(ConnectorName, "Reconcile of "+conceptName)
	}
	return c.i.reconcileIndexAsReport(ctx)
}

func (c connector) Backfill(context.Context, string, string) (memqlsync.BackfillPage, error) {
	return memqlsync.BackfillPage{}, memqlsync.NotImplemented(ConnectorName, "Backfill")
}

func (c connector) Propagate(context.Context, memqlsync.OutboxEntry) (memqlsync.PropagateResult, error) {
	return memqlsync.PropagateResult{}, memqlsync.NotImplemented(ConnectorName, "Propagate")
}

func (c connector) EnsureSubscriptions(context.Context) error {
	return memqlsync.NotImplemented(ConnectorName, "EnsureSubscriptions")
}

// connectorContext stamps the connector actor for a write into the
// mirror.
//
// This is what the mirror write guard admits: v1:shopify:shopifyProduct
// declares @origin("shopify"), so the ONE identity allowed to write it
// is auth.ConnectorActor("shopify"), and row admission matches that
// actor's name against the concept's own declaration.
//
// It also stamps INTERNAL ORIGIN, and the two are not interchangeable:
//
//   - the CONNECTOR ACTOR is what the mirror guard admits. It answers
//     "is SHOPIFY writing", which is the only question that admits a
//     write to a Shopify mirror.
//   - INTERNAL ORIGIN is what a @serverOnly construct requires
//     (memql#2800). upsertShopifyProduct and retireShopifyProduct are
//     @serverOnly precisely BECAUSE the concept is a mirror -- a
//     client-reachable mutation over it is an SDK method that can only
//     fail -- so the connector must present it. auth.CallOrigin's zero
//     value is OriginClient, so a context that does not say otherwise is
//     treated as a client call regardless of which actor it carries.
//
// Relying on the caller's ambient stamp instead would work today (the
// automation dispatcher stamps internal origin for trusted dispatch) and
// would break the moment the connector is driven from anywhere else --
// which epic memql#4378's own runtime does. A forgotten stamp fails
// loudly; an inherited one fails on somebody else's refactor.
//
// Applied per call, on a context this package constructs, which is the
// scoping rule rowauthz_write_guard.go states for every such stamp: the
// marked context is the argument to one Execute and dies there.
func connectorContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = auth.ContextWithConnectorActor(ctx, ConnectorName)
	return auth.ContextWithInternalOrigin(ctx)
}
