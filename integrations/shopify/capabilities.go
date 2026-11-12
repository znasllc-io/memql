package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// capabilities.go -- the connector's DSL-callable surface.
//
// These are how an automation drives ingestion: the dispatcher applies a
// staged delivery, a scheduled automation reconciles a domain and refreshes
// subscriptions, an operator's action starts a backfill. The connector's
// LOGIC lives in the sibling files; this is the seam that lets the DSL run
// it, so that when a domain is backfilled and on what cadence stays a
// declaration rather than a hardcoded loop.

// Integration is the Shopify IntegrationProvider.
type Integration struct {
	connector *Connector
}

// NewIntegration wraps a connector as an integration provider.
func NewIntegration(c *Connector) *Integration { return &Integration{connector: c} }

func (i *Integration) IntegrationName() string { return ConnectorName }

// Connector exposes the connector, for boot wiring.
func (i *Integration) Connector() *Connector { return i.connector }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	base := []memql.IntegrationCapability{
		{
			Name:        "ensureSubscriptions",
			Description: "Register every mirrored webhook topic for every ingesting store at the pinned API version, update the ones that drifted, and remove ours that the allowlist no longer wants. Records the outcome on each store's health. Safe to run on boot and daily. For the scheduled automation (internal origin) or a cluster owner; anybody else is refused before anything runs.",
			Handler:     i.handleEnsureSubscriptions,
		},
		{
			Name:        "runComplianceJobs",
			Description: "Run the privacy jobs whose hold has elapsed: export a customer's data, scrub a redacted customer across every version, purge a redacted shop. Every action is audited. For the scheduled automation (internal origin) or a cluster owner; anybody else is refused before anything runs.",
			Handler:     i.handleRunComplianceJobs,
		},
		{
			Name:        "fetchProduct",
			Description: "Read a mirrored Shopify product by GID or handle. Answers from the mirror, so it costs no Admin API call and no cost points. Tokens never appear in the reply. For the store's owner or a cluster owner; anybody else is refused before the mirror is read.",
			Handler:     i.handleFetchProduct,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store; when omitted, the only configured store for a cluster owner; for anybody else the one store they own, and when they own several, refused as needing a storeId",
				"id":      "string (optional) - the product GID",
				"handle":  "string (optional) - the storefront handle",
			},
		},
		{
			Name:        "shopifyql",
			Description: "Run a ShopifyQL analytics query against a store, for questions the mirror cannot answer. Requires read_reports and protected-customer-data Level 2; refused with a reason below that. For the store's owner or a cluster owner; anybody else is refused before Shopify is asked.",
			Handler:     i.handleShopifyQL,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store to query; when omitted, the only configured store for a cluster owner; for anybody else the one store they own, and when they own several, refused as needing a storeId",
				"query":   "string - the ShopifyQL statement",
			},
		},
		{
			Name:        "commerceSold",
			Description: "What sold in a window, grouped by product or by variant, read entirely from the mirror -- no Admin call and no cost points. Reports units and orders per line, and says so when the walk hit its page cap rather than returning a smaller number. For the store's owner or a cluster owner; anybody else is refused before the mirror is read.",
			Handler:     i.handleCommerceSold,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store; when omitted, the only configured store for a cluster owner; for anybody else the one store they own, and when they own several, refused as needing a storeId",
				"from":    "string (optional) - RFC3339 window start; 30 days back when omitted",
				"to":      "string (optional) - RFC3339 window end; now when omitted",
				"groupBy": "string (optional) - product (default) or variant",
			},
		},
		{
			Name:        "commerceStock",
			Description: "Inventory levels below a threshold at one location. The threshold is applied here because Shopify models inventory as NAMED quantities and a level with no `available` count reads as unknown rather than as a stockout. For the store's owner or a cluster owner; anybody else is refused before the mirror is read.",
			Handler:     i.handleCommerceStock,
			ArgsSchema: map[string]string{
				"storeId":     "string (optional) - the store; when omitted, the only configured store for a cluster owner; for anybody else the one store they own, and when they own several, refused as needing a storeId",
				"locationGid": "string - the location to check",
				"threshold":   "integer (optional) - default 5",
			},
		},
		{
			Name:        "commerceCustomers",
			Description: "Repeat-customer and refund rates for a window, from the mirror. Repeat rate is customers with more than one order over customers with any; refund rate is refunds over orders. For the store's owner or a cluster owner; anybody else is refused before the mirror is read.",
			Handler:     i.handleCommerceCustomers,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store; when omitted, the only configured store for a cluster owner; for anybody else the one store they own, and when they own several, refused as needing a storeId",
				"from":    "string (optional) - RFC3339 window start; 30 days back when omitted",
				"to":      "string (optional) - RFC3339 window end; now when omitted",
			},
		},
		{
			Name:        "commerceCompany",
			Description: "One B2B account: its orders in a window, how many have payment terms still outstanding, and its MemQL-owned credit limit. The two halves come from different systems and the question a rep asks -- can this account order -- needs both. For the store's owner or a cluster owner; anybody else is refused before the mirror is read.",
			Handler:     i.handleCommerceCompany,
			ArgsSchema: map[string]string{
				"storeId":    "string (optional) - the store; when omitted, the only configured store for a cluster owner; for anybody else the one store they own, and when they own several, refused as needing a storeId",
				"companyGid": "string - the mirrored company",
				"from":       "string (optional) - RFC3339 window start; 30 days back when omitted",
				"to":         "string (optional) - RFC3339 window end; now when omitted",
			},
		},
		{
			Name:        "storefrontProbe",
			Description: "Ask one store's Storefront API the three questions a storefront preview can answer: does the catalog read, does a cart accept a line, does a checkoutUrl come back (epic memql#5531, issue memql#5547). Writes nothing and buys nothing -- it creates a cart and reads the URL a shopper would be sent to, and the payment walk itself happens in a browser and is not something this cluster can observe. Returns {storeId, storeDomain, isDevelopment, catalog, cart, checkout}, each of the three {ok, detail, failure, durationMs}. For the store's owner, a cluster owner, or a caller who may read stores; anybody else is refused before Shopify is asked.",
			Handler:     i.handleStorefrontProbe,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store to ask; when omitted, the only configured store for a cluster owner; for anybody else the one store they own, and when they own several, refused as needing a storeId",
			},
		},
		{
			Name:        "storeHealth",
			Description: "Report the status, subscription reconcile time, per-domain sync state and drift counters of each store the caller may read: every store for a cluster owner or a caller who may read stores, a store's owner their own, anybody else none. The read behind the portal's Stores page.",
			Handler:     i.handleStoreHealth,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - one store; when omitted, every store the caller may read",
			},
		},
	}
	base = append(base, i.verdictCapabilities()...)
	base = append(base, i.watchCapabilities()...)
	base = append(base, i.connectCapabilities()...)
	// THE WHOLESALE WRITES ARE APPENDED RATHER THAN LISTED HERE (epic
	// memql#5533). They are the only capabilities in this connector that
	// create B2B objects in a merchant's live store and change what
	// somebody pays, and wholesale_capabilities.go carries the argument for
	// each of them beside the code. Folding them into this list would put
	// five one-line entries here and the reasoning three files away.
	return append(base, i.wholesaleCapabilities()...)
}

// requireInternalOriginOrClusterOwner admits the engine's own call -- the
// scheduled automation, whose actor is a reader and whose step carries
// internal origin because its source is the registered tree -- and a cluster
// owner, and refuses anybody else before anything runs.
func requireInternalOriginOrClusterOwner(ctx context.Context, builtin string) error {
	if auth.OriginFromContext(ctx).IsInternal() || isClusterOwner(ctx) {
		return nil
	}
	return fmt.Errorf("shopify: %s is for a cluster owner", builtin)
}

func (i *Integration) handleEnsureSubscriptions(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOriginOrClusterOwner(ctx, "shopifyEnsureSubscriptions"); err != nil {
		return nil, err
	}
	if i.connector == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	if err := i.connector.EnsureSubscriptions(ctx); err != nil {
		return nil, err
	}
	return resultNode("shopify", map[string]any{"status": "reconciled"})
}

func (i *Integration) handleRunComplianceJobs(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternalOriginOrClusterOwner(ctx, "shopifyRunComplianceJobs"); err != nil {
		return nil, err
	}
	if i.connector == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	ran, err := i.connector.RunDueComplianceJobs(ctx)
	if err != nil {
		return nil, err
	}
	return resultNode("shopify", map[string]any{"status": "ran", "jobs": ran})
}

func (i *Integration) handleFetchProduct(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.mirrorStoreForCaller(ctx, argString(args, "storeId"), "shopifyFetchProduct")
	if err != nil {
		return nil, err
	}
	gid := argString(args, "id")
	handle := argString(args, "handle")
	if gid == "" && handle == "" {
		return nil, fmt.Errorf("shopify: id or handle is required")
	}
	row, found, err := c.mirroredProduct(ctx, store, gid, handle)
	if err != nil {
		return nil, err
	}
	if !found {
		return resultNode("shopify", map[string]any{"status": "not_mirrored", "storeId": store.ID})
	}
	return resultNode("shopify", row)
}

func (i *Integration) handleShopifyQL(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.mirrorStoreForCaller(ctx, argString(args, "storeId"), "shopifyql")
	if err != nil {
		return nil, err
	}
	rows, err := c.ShopifyQL(ctx, store, argString(args, "query"))
	if err != nil {
		return nil, err
	}
	return resultNode("shopify", map[string]any{"status": "ok", "storeId": store.ID, "result": rows})
}

func (i *Integration) handleCommerceSold(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.mirrorStoreForCaller(ctx, argString(args, "storeId"), "commerceSold")
	if err != nil {
		return nil, err
	}
	from, to := c.AnalyticsWindow(argString(args, "from"), argString(args, "to"))
	report, err := c.CommerceSold(ctx, store, from, to, argString(args, "groupBy"))
	if err != nil {
		return nil, err
	}
	return reportNode(report)
}

func (i *Integration) handleCommerceStock(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.mirrorStoreForCaller(ctx, argString(args, "storeId"), "commerceStock")
	if err != nil {
		return nil, err
	}
	location := argString(args, "locationGid")
	if location == "" {
		return nil, fmt.Errorf("shopify: commerceStock needs a locationGid")
	}
	report, err := c.CommerceStock(ctx, store, location, argInt(args, "threshold", 5))
	if err != nil {
		return nil, err
	}
	return reportNode(report)
}

func (i *Integration) handleCommerceCustomers(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.mirrorStoreForCaller(ctx, argString(args, "storeId"), "commerceCustomers")
	if err != nil {
		return nil, err
	}
	from, to := c.AnalyticsWindow(argString(args, "from"), argString(args, "to"))
	report, err := c.CommerceCustomers(ctx, store, from, to)
	if err != nil {
		return nil, err
	}
	return reportNode(report)
}

func (i *Integration) handleCommerceCompany(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.mirrorStoreForCaller(ctx, argString(args, "storeId"), "commerceCompany")
	if err != nil {
		return nil, err
	}
	company := argString(args, "companyGid")
	if company == "" {
		return nil, fmt.Errorf("shopify: commerceCompany needs a companyGid")
	}
	from, to := c.AnalyticsWindow(argString(args, "from"), argString(args, "to"))
	report, err := c.CommerceCompany(ctx, store, company, from, to)
	if err != nil {
		return nil, err
	}
	return reportNode(report)
}

// reportNode marshals a typed report into the single node the DSL reads.
func reportNode(report any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return resultNode("shopify", payload)
}

func (i *Integration) handleStoreHealth(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		return nil, err
	}
	// ONLY THE STORES THE CALLER MAY READ, and never a refusal that names one:
	// every store for a cluster owner or a caller the store tier lets read
	// them, a store's owner their own, anybody else an empty list. Asked
	// before anything but the store rows is read.
	every := isClusterOwner(ctx) || c.mayReadStores(ctx)
	only := argString(args, "storeId")
	out := make([]any, 0, len(stores))
	for _, store := range stores {
		if only != "" && store.ID != only {
			continue
		}
		if !every && !ownsStore(ctx, store) {
			continue
		}
		// Each store is a partition of the mirror: its domains are the rows
		// of its own partition, and no other store's.
		states, err := c.storeSyncStates(ctx, store.ID)
		if err != nil {
			return nil, err
		}
		domains := make([]any, 0, len(states))
		drift := 0
		for _, st := range states {
			drift += mapInt(st, "driftCount")
			domains = append(domains, map[string]any{
				"concept":          mapString(st, "conceptId"),
				"phase":            phaseOf(st),
				"lastAppliedAt":    mapString(st, "lastInboundAt"),
				"lastReconciledAt": mapString(st, "lastReconcileAt"),
				// EACH KEY NAMES WHAT IT CARRIES (epic memql#5009). Three of
				// them did not, and the operator surface reading them is the
				// only consumer, so the repair belongs here rather than in a
				// rename at the render:
				//
				//   staleWrites -> lagSeconds   carried lagSeconds, which is a
				//                               LATENCY in seconds and not a
				//                               count of writes.
				//   tombstoned  -> outboxDepth  carried outboxDepth, the pending
				//                               and failed outbox entries.
				//                               "Tombstoned: 340" for an outbox
				//                               backlog is a wrong number under
				//                               a wrong name.
				//   driftTotal                  carried driftCount -- the SAME
				//                               value as driftLast -- so a
				//                               surface rendering "n / total"
				//                               always printed "n / n" and
				//                               claimed a cumulative figure
				//                               nothing measures. Dropped
				//                               rather than renamed: there is
				//                               no second number to report.
				"driftLast":   mapInt(st, "driftCount"),
				"lagSeconds":  mapInt(st, "lagSeconds"),
				"outboxDepth": mapInt(st, "outboxDepth"),
				"lastError":   mapString(st, "lastError"),
			})
		}
		// The scopes and domains are measured against what this store's own
		// connection asks for: a scope or domain outside it was never
		// requested, and is neither missing nor not granted.
		entry := map[string]any{
			"storeId":            store.ID,
			"domain":             store.Domain,
			"status":             store.Status,
			"apiVersion":         store.APIVersion,
			"mirrorApiVersion":   generated.APIVersion,
			"protectedDataLevel": store.ProtectedDataLevel,
			"scopesGranted":      toAny(store.ScopesGranted),
			"scopesNeeded":       toAny(connectionScopes(store)),
			"scopesMissing":      toAny(scopesNotApproved(store)),
			"domainsNotGranted":  toAny(domainsNotGranted(store)),
			"domainsOutside":     toAny(domainsOutside(store)),
			"fieldsDenied":       fieldsDeniedAny(c.FieldsDenied(store.ID)),
			"health":             store.Health,
			"driftLast":          drift,
			"domains":            domains,
		}
		// Set while the store is disconnected (memql#5638): Shopify reported the
		// app uninstalled, so the store ingests nothing whatever its status
		// says, and its storefronts are served as unavailable.
		entry["uninstalledAt"] = store.UninstalledAt
		// Privacy deliveries refused on this store's per-store URL, read off
		// the audit trail (privacy_refusal.go). Each one is a legal request
		// Shopify will not resend, so a failed read fails the report rather
		// than rendering as "none refused".
		privacy, err := c.privacyDeliveries(ctx, store)
		if err != nil {
			return nil, err
		}
		entry["privacyDeliveries"] = privacy
		if bucket, ok := c.admin.Bucket(store.ID); ok {
			entry["costBucket"] = map[string]any{
				"currentlyAvailable": bucket.CurrentlyAvailable,
				"maximumAvailable":   bucket.MaximumAvailable,
				"restoreRate":        bucket.RestoreRate,
			}
		}
		out = append(out, entry)
	}
	return resultNode("shopify", map[string]any{"status": "ok", "stores": out})
}

// storeSyncStates reads one store's current health: the row of each
// mirrored domain on the store's own partition, BY ID, and only the domains
// that have one.
//
// By id, never by walking syncStatesAll: those rows are the append-only
// history, and every sweep and delivery adds to it, so a reader that walked
// it to find each tip would cost more with every passing day and every store
// connected. Read by id, the cost is the domains times the stores, and exact.
// The id is the runtime's own derivation (memqlsync.StateID), so the row read
// is the one the runtime wrote. The shared row written before stores were
// partitions has an id no store derives, and belongs to no store.
//
// Read through the runtime's own declared query rather than a second one
// of this connector's: v1:platform:syncState belongs to
// component/datasync, and a connector that declared a parallel read of
// it would be a second answer to a question the runtime already answers.
func (c *Connector) storeSyncStates(ctx context.Context, storeID string) ([]map[string]any, error) {
	all := make([]string, 0, len(generated.ApplyOrder))
	for _, concept := range generated.ApplyOrder {
		all = append(all, generated.ConceptID(concept))
	}
	return c.syncStatesOf(ctx, storeID, all)
}

// syncStatesOf is storeSyncStates for the named domains (concept ids) only:
// the store's row of each, by id, and only those that have one.
func (c *Connector) syncStatesOf(ctx context.Context, storeID string, concepts []string) ([]map[string]any, error) {
	out := []map[string]any{}
	for _, concept := range concepts {
		stateID := memqlsync.StateID(concept, ConnectorName, string(memqlsync.DirectionInbound), storeID)
		res, err := c.engine.Execute(operatorContext(ctx), renderCall("syncStateById", map[string]any{"stateId": stateID}))
		if err != nil {
			return nil, fmt.Errorf("shopify: read sync state: %w", err)
		}
		if rows := memql.MaterializeRows(res); len(rows) > 0 && mapString(rows[0], "conceptId") != "" {
			out = append(out, rows[0])
		}
	}
	return out, nil
}

// phaseOf renders the runtime's syncState fields as the one word the
// Stores page shows.
func phaseOf(st map[string]any) string {
	if b, ok := rowValue(st, "paused").(bool); ok && b {
		return "paused"
	}
	if lastErr := mapString(st, "lastError"); lastErr != "" {
		if strings.HasPrefix(lastErr, memqlsync.ErrNotGranted.Error()) {
			// A standing fact about the grant, not a fault: the domain is
			// idle until the grant widens, and the reason is on the row.
			return "not granted"
		}
		if strings.HasPrefix(lastErr, memqlsync.ErrNotRequested.Error()) {
			// Outside the store's connection: never asked for, and
			// nothing to grant.
			return "not requested"
		}
		return "error"
	}
	switch mapString(st, "backfillStatus") {
	case "running", "queued":
		// A queued backfill is one the store is waiting on, and reads as
		// backfilling: the domain is not yet whole.
		return "backfilling"
	case "failed":
		return "error"
	}
	return "idle"
}

// domainsNotGranted names the mirrored domains inside the store's
// connection whose grant reaches none of the scopes for -- the ones the sweep
// skips rather than asks about (scopesMissingFor). What an operator would
// unlock by approving what the connection asks for.
func domainsNotGranted(store Store) []string {
	return sweptDomains(func(spec *generated.TypeSpec) bool {
		return domainInConnection(store, spec) && len(scopesMissingFor(store, spec)) > 0
	})
}

// domainsOutside names the swept domains the store's connection does not
// ask for: never requested, and not an error.
func domainsOutside(store Store) []string {
	return sweptDomains(func(spec *generated.TypeSpec) bool {
		return !domainInConnection(store, spec)
	})
}

// sweptDomains is the concept ids, in apply order, of the domains the sweep
// considers on its own and `keep` accepts. A child materialised with its
// parent, or a singleton, is never swept on its own and never listed.
func sweptDomains(keep func(*generated.TypeSpec) bool) []string {
	var out []string
	for _, concept := range generated.ApplyOrder {
		spec := generated.Types[concept]
		if spec == nil || spec.Reconcile == generated.ReconcileNone {
			continue
		}
		if keep(spec) {
			out = append(out, generated.ConceptID(concept))
		}
	}
	return out
}

func fieldsDeniedAny(in map[string][]string) map[string]any {
	out := map[string]any{}
	for concept, fields := range in {
		out[concept] = toAny(fields)
	}
	return out
}

// mirrorStoreForCaller resolves the store a builtin that reads the mirror or
// asks Shopify names -- or, naming none, the store it defaults to -- and
// refuses, before any mirror read or call to Shopify, a caller who is neither
// that store's owner nor a cluster owner.
//
// The mirror reads run as the operator (F10), and the generated reads'
// actor.isClusterOwner conjunct then admits every one of them; this is
// what keeps a caller to the stores they may act on. There is no
// internal-origin escape: no automation or connector pass reaches these
// builtins, and an automation's own actor is a reader, which this refuses.
func (c *Connector) mirrorStoreForCaller(ctx context.Context, storeID, builtin string) (Store, error) {
	return c.storeForCallerOrDefault(ctx, storeID, builtin, actRefusal, func(s Store) bool { return c.mayActOnStore(ctx, s) })
}

// storeForCallerOrDefault is storeForCaller for a builtin whose storeId may be
// omitted.
//
// A CALLER WHO IS NOT A CLUSTER OWNER AND NAMES NO STORE IS NEVER TOLD HOW MANY
// STORES ARE CONFIGURED: the default is the one store they own; an owner of
// several is asked to name one; anybody else gets the refusal -- the same words
// a store they may not reach answers. A cluster owner may know, and is
// defaulted as resolveStoreArg defaults.
func (c *Connector) storeForCallerOrDefault(ctx context.Context, storeID, builtin, refused string, may func(Store) bool) (Store, error) {
	if storeID != "" {
		return c.storeForCaller(ctx, storeID, builtin, refused, may)
	}
	if isClusterOwner(ctx) {
		return c.resolveStoreArg(ctx, "")
	}
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		return Store{}, err
	}
	var owned []Store
	for _, s := range stores {
		if ownsStore(ctx, s) {
			owned = append(owned, s)
		}
	}
	switch {
	case len(owned) > 1:
		// They own every one of them, so naming them tells nothing new.
		return Store{}, fmt.Errorf("shopify: %s needs a storeId", builtin)
	case len(owned) == 0 || !may(owned[0]):
		return Store{}, fmt.Errorf("shopify: %s %s", builtin, refused)
	}
	return owned[0], nil
}

// resolveStoreArg resolves the store a capability call names, defaulting to
// the only configured one. Defaulting is safe when there is exactly ONE: with
// two, guessing would silently answer about the wrong merchant.
func (c *Connector) resolveStoreArg(ctx context.Context, storeID string) (Store, error) {
	if storeID != "" {
		store, ok := c.stores.ByID(ctx, storeID)
		if !ok {
			return Store{}, fmt.Errorf("shopify: store %q is not configured", storeID)
		}
		return store, nil
	}
	stores, err := c.stores.Stores(ctx)
	if err != nil {
		return Store{}, err
	}
	switch len(stores) {
	case 0:
		return Store{}, fmt.Errorf("shopify: no store is configured")
	case 1:
		return stores[0], nil
	default:
		return Store{}, fmt.Errorf("shopify: %d stores are configured, so storeId is required", len(stores))
	}
}

// mirroredProduct answers a product read out of the mirror.
func (c *Connector) mirroredProduct(ctx context.Context, store Store, gid, handle string) (map[string]any, bool, error) {
	spec := generated.Types["product"]
	if spec == nil {
		return nil, false, fmt.Errorf("shopify: the product concept is not in this build")
	}
	// operatorContext, not connectorContext: the generated mirror reads
	// filter on actor.isClusterOwner, which a connector actor never is, so
	// under its own identity the connector read none of the rows it wrote
	// (F10). TestTheConnectorReadsBackTheMirrorRowsItWrote holds it.
	if gid != "" {
		res, err := c.engine.Execute(operatorContext(ctx), renderCall(spec.ByGidFn, map[string]any{"storeId": store.ID, "gid": gid}))
		if err != nil {
			return nil, false, err
		}
		rows := memql.MaterializeRows(res)
		if len(rows) == 0 {
			return nil, false, nil
		}
		return rows[0], true, nil
	}
	// BY HANDLE, TO THE END. The generated read has no handle argument and
	// pages 100 rows newest first, so a product outside the newest hundred is
	// on a later page: every page is read until one has it or there are no
	// more. A cursor that does not move, or a mirror past the page bound, is
	// said rather than answered as not mirrored.
	opCtx := operatorContext(ctx)
	call := renderCall(spec.ForStoreFn, map[string]any{"storeId": store.ID})
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < liveRowPageCap; page++ {
		pageCtx := opCtx
		if cursor != "" {
			pageCtx = memql.ContextWithCursor(opCtx, cursor)
		}
		res, err := c.engine.Execute(pageCtx, call)
		if err != nil {
			return nil, false, err
		}
		rows := memql.MaterializeRows(res)
		for _, row := range rows {
			if mapString(row, "handle") == handle {
				return row, true, nil
			}
		}
		next := ""
		if res != nil {
			if meta := res.GetMeta(); meta != nil {
				next = strings.TrimSpace(meta.Cursor)
			}
		}
		if next == "" || len(rows) == 0 {
			return nil, false, nil
		}
		if next == cursor || seen[next] {
			return nil, false, fmt.Errorf("shopify: read mirrored products by handle: the cursor did not advance")
		}
		seen[next] = true
		cursor = next
	}
	return nil, false, fmt.Errorf("shopify: read mirrored products by handle: the mirror holds more than %d pages", liveRowPageCap)
}

const shopifyQLQuery = `query ShopifyQL($query: String!) {
  shopifyqlQuery(query: $query) {
    __typename
    ... on TableResponse {
      tableData {
        unformattedData
        rowData
        columns { name dataType displayName }
      }
    }
    parseErrors { code message range { start { line character } end { line character } } }
  }
}`

// ShopifyQL runs an ad-hoc analytics query.
//
// Refused below Level 2 approval, on OUR side, with a reason. Shopify would
// refuse it too, with a 403 that names neither the scope nor the approval
// level -- and an operator reading that has no way to tell a missing scope
// from a query they typed wrong.
func (c *Connector) ShopifyQL(ctx context.Context, store Store, query string) (any, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("shopify: shopifyql needs a query")
	}
	if !store.ProtectedLevelAtLeast(ProtectedLevel2) {
		return nil, fmt.Errorf("shopify: store %q is approved at protected-data level %q; shopifyqlQuery needs %q plus read_reports",
			store.ID, store.ProtectedDataLevel, ProtectedLevel2)
	}
	resp, err := c.adminCall(ctx, store, shopifyQLQuery, "ShopifyQL", map[string]any{"query": query})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Data, &out); err != nil {
		return nil, err
	}
	return out["shopifyqlQuery"], nil
}

// resultNode wraps a capability's answer as the single node the DSL reads.
func resultNode(kind string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:        "integration:" + kind + ":result",
		Concept:   "integration:" + kind + ":result",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	switch v := args[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

// argInt reads a numeric capability arg.
//
// THE CALLER'S DEFAULT, out of range (memql#4779). Its caller is a low-stock
// threshold with `5` already named at the call site, and a threshold of 2^63
// reports every location in the store as low.
func argInt(args map[string]any, key string, fallback int) int {
	if args == nil {
		return fallback
	}
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return num.Int64Or(v, fallback)
	case float64:
		return num.Float64Or(v, fallback)
	}
	return fallback
}

// handleStorefrontProbe asks one store the three questions a preview can answer.
//
// # It is a READ of somebody else's system, and it changes nothing here
//
// It writes no row. The observation rows these results become are written by
// the package that owns them (integrations/sitepreview), which reaches this
// through a builtin over the engine -- the seam component/packages already uses
// to reach customDomainAdd. That split is what keeps the module taxonomy
// honest: the call to Shopify lives in the package classified as the one that
// talks to Shopify, and the rows live with the concept that declares them.
//
// # A cart IS created, and saying so is part of the contract
//
// `cartCreate` leaves a cart on the store. That is not a side effect to hide:
// it is the thing being observed, and it is why this is pointed at a
// DEVELOPMENT store by a guard one level up rather than being safe to run
// anywhere. Nothing is paid for and no order is placed.
//
// # The store's own development flag is reported back
//
// The guard that refuses a preview against the store shoppers reach lives in
// component/sitepreview, and this capability does not repeat it -- a second
// copy of a safety rule is a copy that drifts. What it does do is REPORT the
// flag, so a caller that reached here by some path with no guard in front of it
// can still see what it just talked to.
func (i *Integration) handleStorefrontProbe(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	if c == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	store, err := c.storeForCallerOrDefault(ctx, argString(args, "storeId"), "storefrontProbe", readRefusal,
		func(s Store) bool { return c.mayReadStore(ctx, s) })
	if err != nil {
		return nil, err
	}
	endpoint, err := StorefrontEndpoint(store.Domain, store.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("shopify: store %q cannot be reached for a storefront probe: %w", store.ID, err)
	}
	token, err := c.stores.Secret(ctx, store.StorefrontTokenRef)
	if err != nil {
		return nil, fmt.Errorf("shopify: store %q has no resolvable Storefront token (%s): %w", store.ID, store.StorefrontTokenRef, err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("shopify: store %q names no Storefront token, so a storefront cannot read its catalog at all", store.ID)
	}

	report := StorefrontProbe(ctx, endpoint, token)
	return resultNode("shopify", map[string]any{
		"storeId":       store.ID,
		"storeDomain":   store.Domain,
		"isDevelopment": store.IsDevelopment,
		// THE THREE KEYS ARE NOT SPELLED HERE. They are the wire between this
		// capability and the one that turns its answers into rows, and a wire
		// spelled at both ends drifts -- silently, into a panel that reports
		// three unmeasured steps against a store that answered all three. The
		// one spelling is component/sitepreview/wire.go.
		memql.PreviewStepCatalog:  probeStepPayload(report.Catalog),
		memql.PreviewStepCart:     probeStepPayload(report.Cart),
		memql.PreviewStepCheckout: probeStepPayload(report.Checkout),
	})
}

func probeStepPayload(s ProbeStep) map[string]any {
	return map[string]any{
		"ok":         s.OK,
		"detail":     s.Detail,
		"failure":    s.Failure,
		"durationMs": s.DurationMs,
	}
}
