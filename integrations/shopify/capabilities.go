package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	return []memql.IntegrationCapability{
		{
			Name:        "ensureSubscriptions",
			Description: "Register every mirrored webhook topic for every ingesting store at the pinned API version, update the ones that drifted, and remove ours that the allowlist no longer wants. Records the outcome on each store's health. Safe to run on boot and daily.",
			Handler:     i.handleEnsureSubscriptions,
		},
		{
			Name:        "runComplianceJobs",
			Description: "Run the privacy jobs whose hold has elapsed: export a customer's data, scrub a redacted customer across every version, purge a redacted shop. Every action is audited.",
			Handler:     i.handleRunComplianceJobs,
		},
		{
			Name:        "fetchProduct",
			Description: "Read a mirrored Shopify product by GID or handle. Answers from the mirror, so it costs no Admin API call and no cost points. Tokens never appear in the reply.",
			Handler:     i.handleFetchProduct,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store; the only configured store when omitted",
				"id":      "string (optional) - the product GID",
				"handle":  "string (optional) - the storefront handle",
			},
		},
		{
			Name:        "shopifyql",
			Description: "Run a ShopifyQL analytics query against a store, for questions the mirror cannot answer. Requires read_reports and protected-customer-data Level 2; refused with a reason below that.",
			Handler:     i.handleShopifyQL,
			ArgsSchema: map[string]string{
				"storeId": "string - the store to query",
				"query":   "string - the ShopifyQL statement",
			},
		},
		{
			Name:        "commerceSold",
			Description: "What sold in a window, grouped by product or by variant, read entirely from the mirror -- no Admin call and no cost points. Reports units and orders per line, and says so when the walk hit its page cap rather than returning a smaller number.",
			Handler:     i.handleCommerceSold,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store; the only configured one when omitted",
				"from":    "string (optional) - RFC3339 window start; 30 days back when omitted",
				"to":      "string (optional) - RFC3339 window end; now when omitted",
				"groupBy": "string (optional) - product (default) or variant",
			},
		},
		{
			Name:        "commerceStock",
			Description: "Inventory levels below a threshold at one location. The threshold is applied here because Shopify models inventory as NAMED quantities and a level with no `available` count reads as unknown rather than as a stockout.",
			Handler:     i.handleCommerceStock,
			ArgsSchema: map[string]string{
				"storeId":     "string (optional) - the store",
				"locationGid": "string - the location to check",
				"threshold":   "integer (optional) - default 5",
			},
		},
		{
			Name:        "commerceCustomers",
			Description: "Repeat-customer and refund rates for a window, from the mirror. Repeat rate is customers with more than one order over customers with any; refund rate is refunds over orders.",
			Handler:     i.handleCommerceCustomers,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - the store",
				"from":    "string (optional) - RFC3339 window start; 30 days back when omitted",
				"to":      "string (optional) - RFC3339 window end; now when omitted",
			},
		},
		{
			Name:        "commerceCompany",
			Description: "One B2B account: its orders in a window, how many have payment terms still outstanding, and its MemQL-owned credit limit. The two halves come from different systems and the question a rep asks -- can this account order -- needs both.",
			Handler:     i.handleCommerceCompany,
			ArgsSchema: map[string]string{
				"storeId":    "string (optional) - the store",
				"companyGid": "string - the mirrored company",
				"from":       "string (optional) - RFC3339 window start; 30 days back when omitted",
				"to":         "string (optional) - RFC3339 window end; now when omitted",
			},
		},
		{
			Name:        "storeHealth",
			Description: "Report every configured store's status, subscription reconcile time, per-domain sync state and drift counters. The read behind the portal's Stores page.",
			Handler:     i.handleStoreHealth,
			ArgsSchema: map[string]string{
				"storeId": "string (optional) - one store; every store when omitted",
			},
		},
	}
}

func (i *Integration) handleEnsureSubscriptions(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.connector == nil {
		return resultNode("shopify", map[string]any{"status": "unconfigured"})
	}
	if err := i.connector.EnsureSubscriptions(ctx); err != nil {
		return nil, err
	}
	return resultNode("shopify", map[string]any{"status": "reconciled"})
}

func (i *Integration) handleRunComplianceJobs(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
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
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
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
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
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
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
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
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
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
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
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
	store, err := c.resolveStoreArg(ctx, argString(args, "storeId"))
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
	states, err := c.syncStates(ctx)
	if err != nil {
		return nil, err
	}
	only := argString(args, "storeId")
	out := make([]any, 0, len(stores))
	for _, store := range stores {
		if only != "" && store.ID != only {
			continue
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
		entry := map[string]any{
			"storeId":            store.ID,
			"domain":             store.Domain,
			"status":             store.Status,
			"apiVersion":         store.APIVersion,
			"mirrorApiVersion":   generated.APIVersion,
			"protectedDataLevel": store.ProtectedDataLevel,
			"scopesGranted":      toAny(store.ScopesGranted),
			"scopesNeeded":       toAny(generated.Scopes),
			"scopesMissing":      toAny(missingScopes(store.ScopesGranted)),
			"health":             store.Health,
			"driftLast":          drift,
			"domains":            domains,
		}
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

// syncStates reads the runtime's health rows for this connector.
//
// Read through the runtime's own declared query rather than a second one
// of this connector's: v1:platform:syncState belongs to
// component/datasync, and a connector that declared a parallel read of
// it would be a second answer to a question the runtime already answers.
func (c *Connector) syncStates(ctx context.Context) ([]map[string]any, error) {
	res, err := c.engine.Execute(connectorContext(ctx), renderCall("syncStatesAll", map[string]any{"connector": ConnectorName}))
	if err != nil {
		return nil, fmt.Errorf("shopify: read sync state: %w", err)
	}
	rows := memql.MaterializeRows(res)
	// The rows are the append-only history, newest first, and the row id
	// is deterministic per domain -- so the TIP of each id is that
	// domain's current health and everything behind it is timeline.
	seen := map[string]bool{}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		concept := mapString(r, "conceptId")
		if concept == "" || seen[concept] {
			continue
		}
		seen[concept] = true
		out = append(out, r)
	}
	return out, nil
}

// phaseOf renders the runtime's syncState fields as the one word the
// Stores page shows.
func phaseOf(st map[string]any) string {
	if b, ok := rowValue(st, "paused").(bool); ok && b {
		return "paused"
	}
	if mapString(st, "lastError") != "" {
		return "error"
	}
	switch mapString(st, "backfillStatus") {
	case "running":
		return "backfilling"
	case "failed":
		return "error"
	}
	return "idle"
}

// missingScopes is the allowlist's needs minus what the store granted.
//
// Reported rather than enforced: a missing scope makes Shopify return null
// for the fields it covers, so the mirror is quietly incomplete rather than
// broken. Naming the gap is the difference between "the customer has no
// phone number" and "we were never allowed to see it".
func missingScopes(granted []string) []string {
	if len(granted) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, s := range granted {
		have[s] = true
	}
	var missing []string
	for _, s := range generated.Scopes {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	return missing
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
	if gid != "" {
		res, err := c.engine.Execute(connectorContext(ctx), renderCall(spec.ByGidFn, map[string]any{"storeId": store.ID, "gid": gid}))
		if err != nil {
			return nil, false, err
		}
		rows := memql.MaterializeRows(res)
		if len(rows) == 0 {
			return nil, false, nil
		}
		return rows[0], true, nil
	}
	res, err := c.engine.Execute(connectorContext(ctx), renderCall(spec.ForStoreFn, map[string]any{"storeId": store.ID}))
	if err != nil {
		return nil, false, err
	}
	for _, row := range memql.MaterializeRows(res) {
		if mapString(row, "handle") == handle {
			return row, true, nil
		}
	}
	return nil, false, nil
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
