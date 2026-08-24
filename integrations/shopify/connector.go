package shopify

// The Shopify CONNECTOR: a complete, generated mirror of a store, the
// origins a wholesale business needs on top of it, and the boundary of
// what nobody can mirror (epic memql#4389 / J1).
//
// It is the J1 fill of the surface J0 (epic memql#4378) left declared:
// Apply widened from one thin product index to 65 generated concepts,
// and Backfill, Propagate and EnsureSubscriptions -- which returned the
// contract's typed not-implemented error -- are implemented here.
//
// ONE CONNECTOR, MANY STORES. Everything store-specific -- the
// credentials, the API version, the webhook secret, the pause switch --
// lives on a v1:shopify:store row, so adding a second store is an
// operator action rather than a deployment. That is why this type holds
// a StoreRegistry rather than a Config.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// ConnectorName is the name concepts write in @origin, the prefix of
// every inbound source this connector owns, and the subject of the actor
// its writes run under. Spelled once.
const ConnectorName = "shopify"

// init declares the connector NAME.
//
// Declaration is separate from binding, and the split is what makes the
// engine's boot check possible: Init runs before integrations are wired
// (app/build_*.go: config -> database -> engine -> integrations), so a
// registry holding only live instances would be empty exactly when the
// check reads it and would refuse every @origin in the tree.
func init() {
	memqlsync.Declare(ConnectorName)
}

// connectorContext stamps the identity every read and write in this
// package runs under.
//
// Two stamps, and they are not interchangeable:
//
//   - the CONNECTOR ACTOR is what the mirror write guard admits. It
//     answers "is SHOPIFY writing", which is the only question that
//     admits a write to a Shopify mirror.
//   - INTERNAL ORIGIN is what a @serverOnly construct requires
//     (memql#2800). Every generated mirror write is @serverOnly
//     precisely BECAUSE the concept is a mirror, and auth.CallOrigin's
//     zero value is OriginClient -- so a context that does not say
//     otherwise is treated as a client call whatever actor it carries.
func connectorContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = auth.ContextWithConnectorActor(ctx, ConnectorName)
	return auth.ContextWithInternalOrigin(ctx)
}

// Connector is the Shopify half of the data-origins contract.
type Connector struct {
	engine memql.IntegrationEngineAccess
	logger *slog.Logger
	stores *StoreRegistry
	admin  *AdminClient
	now    func() time.Time

	// db is the raw handle the two compliance jobs need. Everything else
	// goes through the engine; redaction and purge cannot, because
	// "every version of every row" is exactly what an append-only model
	// will not let you touch.
	db func() *sql.DB

	// applied is the process-local short-circuit for redelivered
	// webhooks. See apply.go.
	applied *applied
	// bulkReady records which bulk operations Shopify has said are done.
	bulkReady *bulkReadySet
	// definitions remembers which metafield definitions have been
	// created, per store.
	definitions *definitionCache

	// deliver overrides the webhook delivery URL. Tests only.
	deliver func(store Store) string
}

// NewConnector builds the connector.
func NewConnector(engine memql.IntegrationEngineAccess, logger *slog.Logger, stores *StoreRegistry, admin *AdminClient) *Connector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Connector{
		engine:      engine,
		logger:      logger,
		stores:      stores,
		admin:       admin,
		now:         time.Now,
		applied:     newApplied(),
		bulkReady:   &bulkReadySet{applied: newApplied()},
		definitions: newDefinitionCache(),
	}
}

// WithDatabase attaches the raw handle the compliance jobs need.
func (c *Connector) WithDatabase(db func() *sql.DB) *Connector {
	c.db = db
	return c
}

// Stores exposes the registry, for the seed and the capability surface.
func (c *Connector) Stores() *StoreRegistry { return c.stores }

// Name implements sync.Connector.
func (c *Connector) Name() string { return ConnectorName }

// Domains lists every mirrored concept plus the MemQL-origin concepts
// this connector pushes. The runtime reads it to schedule reconciliation
// and to route outbox entries.
func (c *Connector) Domains() []memqlsync.DomainSpec {
	out := make([]memqlsync.DomainSpec, 0, len(generated.ApplyOrder)+len(originDomains))
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec == nil {
			continue
		}
		out = append(out, memqlsync.DomainSpec{
			Concept: generated.ConceptID(concept),
			// The ORIGIN's own version, which the runtime compares
			// against the stored row before applying. Every mirrored
			// concept carries it in `updatedAt`; for a type Shopify
			// publishes no version for, the connector stamps the fetch
			// time into that field and the guard degrades to
			// last-write-wins -- recorded on the TypeSpec rather than
			// discovered at runtime.
			VersionField:      "updatedAt",
			ReconcileInterval: parseCadence(spec.Cadence, spec.Reconcile),
			Direction:         memqlsync.DirectionInbound,
		})
	}
	for _, concept := range originDomains {
		out = append(out, memqlsync.DomainSpec{
			Concept:   concept,
			Direction: memqlsync.DirectionOutbound,
		})
	}
	return out
}

// parseCadence turns the allowlist's duration string into the interval
// the runtime schedules on. A domain that reconciles neither way returns
// zero, which the runtime reads as "operator-driven only".
func parseCadence(cadence, mode string) time.Duration {
	if mode == generated.ReconcileNone {
		return 0
	}
	if cadence != "" {
		if d, err := time.ParseDuration(cadence); err == nil {
			return d
		}
	}
	// An updated_at domain has no cadence in the allowlist because the
	// filter makes the sweep cheap; hourly is frequent enough to bound
	// the drift window and far below the cost of a re-list.
	return time.Hour
}

// InboundSource resolves a webhook source this connector owns.
//
// It is NOT on the sync.Connector interface: only a multi-tenant
// connector needs one, and putting it there would oblige every
// implementation to answer a question most have no opinion about. The
// receiver type-asserts for it (component/inbound/handler.go), which is
// the same shape Go's own optional interfaces take.
//
// The name is "shopify-<storeId>", and the store's row carries the
// secret. A name for a store that does not exist reports false, which
// lets the receiver answer 404 -- an unknown source and an unconfigured
// one are deliberately indistinguishable from outside.
func (c *Connector) InboundSource(ctx context.Context, name string) (memqlsync.InboundSource, bool) {
	if c == nil || c.stores == nil {
		return memqlsync.InboundSource{}, false
	}
	prefix := ConnectorName + "-"
	if !strings.HasPrefix(name, prefix) {
		return memqlsync.InboundSource{}, false
	}
	store, ok := c.stores.ByID(ctx, strings.TrimPrefix(name, prefix))
	if !ok {
		return memqlsync.InboundSource{}, false
	}
	return c.stores.InboundSourceFor(ctx, store)
}

// StoreFor resolves the store a staged delivery belongs to.
//
// The SOURCE NAME is authoritative and the shop-domain header is the
// fallback, in that order and not the other way round: the source
// decided which secret verified the signature, so trusting a header over
// it would let a delivery signed for one store be attributed to another.
func (c *Connector) StoreFor(ctx context.Context, req memqlsync.InboundRequest) (Store, bool) {
	prefix := ConnectorName + "-"
	if strings.HasPrefix(req.Source, prefix) {
		if store, ok := c.stores.ByID(ctx, strings.TrimPrefix(req.Source, prefix)); ok {
			return store, true
		}
	}
	if domain := header(req, HeaderShopDomain); domain != "" {
		return c.stores.ByDomain(ctx, domain)
	}
	return Store{}, false
}

// header reads a delivery header case-insensitively.
func header(req memqlsync.InboundRequest, name string) string {
	if req.Headers == nil {
		return ""
	}
	return req.Headers[strings.ToLower(name)]
}

// adminCall runs one Admin operation for a store, resolving the token.
func (c *Connector) adminCall(ctx context.Context, store Store, document, operation string, variables map[string]any) (*AdminResponse, error) {
	token, err := c.stores.AdminToken(ctx, store)
	if err != nil {
		return nil, err
	}
	if store.APIVersion != "" && store.APIVersion != generated.APIVersion {
		// Refused rather than attempted. A store pinned to a different
		// version answers with fields the concepts do not declare and
		// omits fields they require, and the resulting mirror is wrong
		// in a way no error message would explain.
		return nil, fmt.Errorf("shopify: store %q is pinned to Admin API %s but the mirror was generated from %s -- regenerate (see the quarterly bump) or repin the store",
			store.ID, store.APIVersion, generated.APIVersion)
	}
	return c.admin.Do(ctx, storeWithVersion(store), token, document, operation, variables)
}

// storeWithVersion fills in the pinned version for a store row that has
// none, so a store an operator created without one still resolves an
// endpoint.
func storeWithVersion(s Store) Store {
	if s.APIVersion == "" {
		s.APIVersion = generated.APIVersion
	}
	return s
}

// deliveryURL is where Shopify should POST this store's webhooks.
//
// Composed from MEMQL_DOMAIN through the SAME front-door helper the
// Ingress generator and the issuer derivation use. A second spelling of
// the api host would register subscriptions pointing at a hostname
// nothing is served at, and Shopify would delete them after eight
// failures -- a store that silently stops updating, with every manifest
// looking correct.
func (c *Connector) deliveryURL(store Store) string {
	if c.deliver != nil {
		return c.deliver(store)
	}
	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	host := frontdoor.RoleHost(frontdoor.RoleAPI, domain)
	return "https://" + host + "/inbound/" + memqlsync.SourceName(ConnectorName, store.ID)
}

// originDomains are the MemQL-authored concepts this connector pushes to
// Shopify. Declared here rather than derived, because the push channel
// is a closed set by design (decision D7): every entry is a domain
// somebody decided it is safe to write into a live store.
var originDomains = []string{
	"v1:commerce:productContent",
	"v1:commerce:customerNote",
	"v1:commerce:companyLocationNote",
	"v1:commerce:creditLimit",
}

// Compile-time proof that this satisfies the contract. Without it the
// interface is only checked where a *Connector is passed as one, and the
// registry takes it through a factory -- so a missing method would
// surface at boot rather than at build.
var _ memqlsync.Connector = (*Connector)(nil)
