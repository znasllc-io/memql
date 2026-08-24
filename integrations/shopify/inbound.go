package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// Action is the thin-index write the webhook / reconcile decided on.
// skip = not our delivery or unconfigured (never invent).
// upsert = Storefront/Admin returned the product (three fields only).
// retire = Shopify said the product is gone; update a row we already
// had. A miss does not create a row.
type Action string

const (
	ActionSkip   Action = "skip"
	ActionUpsert Action = "upsert"
	ActionRetire Action = "retire"
)

const (
	productGIDPrefix     = "gid://shopify/Product/"
	indexDecisionConcept = "integration:shopify:indexDecision"
)

// Decision is the pure apply/reconcile result. Tests pin this; the
// capability persists only after a successful fetch (upsert) or a
// confirmed miss (retire).
type Decision struct {
	Action Action
	Reason string
	GID    string
	Product
}

// ParseProductDelivery extracts a product GID (and optional handle
// hint) from a Shopify webhook body. ok is false for non-product
// deliveries (orders, campaigns feedback, empty). Handle is a hint
// only -- merchandising still comes from FetchProduct.
func ParseProductDelivery(source, topic string, body []byte) (gid, handle string, ok bool) {
	src := strings.ToLower(strings.TrimSpace(source))
	top := strings.ToLower(strings.TrimSpace(topic))
	shopifyish := src == "shopify" || strings.HasPrefix(src, "shopify") ||
		strings.HasPrefix(top, "products/")

	var raw map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return "", "", false
		}
	}
	if raw == nil {
		return "", "", false
	}

	gid = firstProductGID(raw)
	handle = stringField(raw, "handle")
	if gid != "" {
		return gid, handle, true
	}

	// REST delete/create sometimes only has a numeric id. Require a
	// shopify source/topic so a random JSON {"id": 1} is not invented
	// as a product.
	if !shopifyish {
		return "", "", false
	}
	if looksLikeOrder(raw) {
		return "", "", false
	}
	if n := numericID(raw["id"]); n != "" {
		return productGIDPrefix + n, handle, true
	}
	return "", "", false
}

func firstProductGID(raw map[string]any) string {
	for _, key := range []string{"admin_graphql_api_id", "gid", "id"} {
		s := stringField(raw, key)
		if strings.HasPrefix(s, productGIDPrefix) {
			return s
		}
	}
	return ""
}

func stringField(raw map[string]any, key string) string {
	v, ok := raw[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func numericID(v any) string {
	switch t := v.(type) {
	case json.Number:
		s := t.String()
		if s != "" && s != "0" {
			return s
		}
	case float64:
		if t > 0 && t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
	case int:
		if t > 0 {
			return strconv.Itoa(t)
		}
	case int64:
		if t > 0 {
			return strconv.FormatInt(t, 10)
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" || strings.HasPrefix(s, "gid://") {
			return ""
		}
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return s
		}
	}
	return ""
}

func looksLikeOrder(raw map[string]any) bool {
	if _, ok := raw["line_items"]; ok {
		return true
	}
	for _, key := range []string{"admin_graphql_api_id", "gid"} {
		s := stringField(raw, key)
		if strings.HasPrefix(s, "gid://shopify/Order/") {
			return true
		}
	}
	return false
}

func isProductNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "product not found")
}

// DecideApply turns a FetchProduct result into an index write.
// A miss retires (does not invent). Any other fetch error is not a
// miss -- the caller must not retire on a transport failure.
func DecideApply(fetched Product, fetchErr error, gid string) (Decision, error) {
	gid = strings.TrimSpace(gid)
	if fetchErr == nil && fetched.ID != "" {
		return Decision{
			Action:  ActionUpsert,
			Reason:  "storefront hit",
			GID:     fetched.ID,
			Product: fetched,
		}, nil
	}
	if isProductNotFound(fetchErr) {
		if gid == "" {
			return Decision{Action: ActionSkip, Reason: "miss without gid"}, nil
		}
		return Decision{Action: ActionRetire, Reason: "product not found", GID: gid}, nil
	}
	if fetchErr != nil {
		return Decision{}, fetchErr
	}
	return Decision{Action: ActionSkip, Reason: "empty fetch"}, nil
}

func decisionNode(d Decision) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(map[string]any{
		"action":           string(d.Action),
		"reason":           d.Reason,
		"id":               d.GID,
		"handle":           d.Handle,
		"availableForSale": d.AvailableForSale,
	})
	if err != nil {
		return nil, fmt.Errorf("shopify: marshal decision: %w", err)
	}
	id := d.GID
	if id == "" {
		id = "skip"
	}
	return []memorynodes.MemoryNode{{
		ID:      id,
		Concept: indexDecisionConcept,
		Type:    memorynodes.NodeTypeObject,
		Payload: payload,
	}}, nil
}

func skipNode(reason string) ([]memorynodes.MemoryNode, error) {
	return decisionNode(Decision{Action: ActionSkip, Reason: reason})
}

func (i *Integration) applyFetched(ctx context.Context, gid, handle string) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.client == nil {
		return skipNode("shopify unconfigured")
	}
	p, err := i.client.FetchProduct(ctx, gid, handle)
	d, err := DecideApply(p, err, gid)
	if err != nil {
		return nil, err
	}
	if err := i.commit(ctx, d); err != nil {
		return nil, err
	}
	return decisionNode(d)
}

func (i *Integration) commit(ctx context.Context, d Decision) error {
	if i == nil || i.engine == nil {
		return nil
	}
	// v1:shopify:shopifyProduct declares @origin("shopify") (epic
	// memql#4378), which makes it a MIRROR: read-only to users, agents,
	// tools and raw inserts, and writable only by this connector under
	// its own actor. Every write below therefore goes out on a
	// connector-stamped context -- without it the engine refuses with
	// mirror_write_refused, which is exactly the protection working.
	ctx = connectorContext(ctx)
	switch d.Action {
	case ActionUpsert:
		q := fmt.Sprintf(
			`upsertShopifyProduct(productId: %s, handle: %s, availableForSale: %v)`,
			quoteDSL(d.Product.ID), quoteDSL(d.Product.Handle), d.Product.AvailableForSale,
		)
		_, err := i.engine.Execute(ctx, q)
		return err
	case ActionRetire:
		q := fmt.Sprintf(`retireShopifyProduct(productId: %s)`, quoteDSL(d.GID))
		_, err := i.engine.Execute(ctx, q)
		return err
	default:
		return nil
	}
}

func quoteDSL(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// applyFetchedAsMirrorWrites is applyFetched in the contract's
// vocabulary: it performs the same fetch-decide-commit and DESCRIBES
// what it wrote, rather than returning the builtin's node shape.
//
// The write still goes through commit -- the engine mutations carry the
// concept's field contract, and duplicating them here would be a second
// definition of what a thin-index row is. The MirrorWrite is what the
// runtime records and audits.
func (i *Integration) applyFetchedAsMirrorWrites(ctx context.Context, gid, handle string, version time.Time) ([]memqlsync.MirrorWrite, error) {
	if i == nil || i.client == nil {
		return nil, nil
	}
	p, err := i.client.FetchProduct(ctx, gid, handle)
	d, err := DecideApply(p, err, gid)
	if err != nil {
		return nil, err
	}
	if err := i.commit(ctx, d); err != nil {
		return nil, err
	}
	switch d.Action {
	case ActionUpsert:
		return []memqlsync.MirrorWrite{{
			Concept: ProductConcept,
			RowId:   d.Product.ID,
			Payload: map[string]any{
				"handle":           d.Product.Handle,
				"availableForSale": d.Product.AvailableForSale,
				"present":          true,
			},
			Version: version.UTC().Format(time.RFC3339Nano),
		}}, nil
	case ActionRetire:
		return []memqlsync.MirrorWrite{{
			Concept: ProductConcept,
			RowId:   d.GID,
			Version: version.UTC().Format(time.RFC3339Nano),
			Retire:  true,
		}}, nil
	default:
		// A skip. Zero writes is a normal outcome, not a failure: the
		// dispatcher offers every staged row to the connector its source
		// names, and most of them are not products.
		return nil, nil
	}
}

// reconcileSweepWindow is how many present rows one reconcile sweep
// covers. It matches the `paginate 50` on shopifyProducts, which is what
// the scheduled reconcile has swept since memql#4137 -- the contract
// preserves the existing behaviour rather than widening it as a side
// effect (a wider sweep is a Shopify rate-limit decision, not a
// refactoring one).
const reconcileSweepWindow = 50

// reconcileIndexAsReport is reconcileIndex in the contract's
// vocabulary. It drives the same sweep -- re-fetch every present row, a
// hit upserts and a miss retires, nothing new invented -- and counts
// what it found.
//
// Drift here is "the origin disagreed with the mirror", which for this
// thin index means the fetch produced a different decision than the row
// already held. The sweep heals as it goes, so Healed tracks Drifted;
// the two are separate fields because a connector that could not heal
// what it found must be able to say so, and J1's wider domains will.
func (i *Integration) reconcileIndexAsReport(ctx context.Context) (memqlsync.ReconcileReport, error) {
	var report memqlsync.ReconcileReport
	if i == nil || i.client == nil || i.engine == nil {
		return report, nil
	}
	// A RAW concept read, not the authored shopifyProducts query, and the
	// reason is a real constraint rather than a preference.
	//
	// shopifyProducts is the OPERATOR read: it carries an explicit
	// actor.isClusterOwner==true conjunct, which is what the row-authz land
	// gate requires of every authored query over a tier-declaring concept
	// (component/memql/rowauthz_enforce_gate_test.go). A connector is not an
	// operator and does not satisfy it, so reading through that query returns
	// ZERO ROWS -- an empty result, not an error -- and the sweep reports a
	// clean run over a mirror it never saw.
	//
	// Writing a second authored query without the conjunct is what that gate
	// exists to refuse, and rightly: a reader of the DSL would see an
	// un-gated read of a clusterOwner concept and could not tell it was
	// deliberate. So the connector's internal sweep goes through the same
	// seam the generic concept browse uses -- a raw query string.
	//
	// A compound filter is not a top-level `concept==<id>` equality, so the
	// plan binds NO concept and filter injection does nothing. The guard is
	// therefore the ROW GATE, which decides each row from its own concept's
	// declaration and is the mechanism that knows about connectors: it
	// admits ConnectorActor("shopify") here and denies every ordinary
	// caller issuing the same string. Pinned by
	// component/memql/connector_sweep_query_test.go, which asserts the
	// binding, the absence of injection, and both sides of the row gate --
	// so a change to any of the three fails there rather than turning this
	// sweep into a silent no-op.
	//
	// The window matches what shopifyProducts paginates, so the sweep covers
	// the same rows it covered before the connector contract existed.
	res, err := i.engine.Execute(connectorContext(ctx), fmt.Sprintf(
		`sort(paginate(concept==%s && present==true, %d), "createdAt", "desc")`,
		ProductConcept, reconcileSweepWindow))
	if err != nil {
		return report, fmt.Errorf("shopify: list index: %w", err)
	}
	for _, id := range productIDsFromResult(res) {
		report.Checked++
		writes, err := i.applyFetchedAsMirrorWrites(ctx, id, "", time.Now().UTC())
		if err != nil {
			return report, err
		}
		if len(writes) > 0 {
			report.Drifted++
			report.Healed++
		}
	}
	return report, nil
}

// SetEngine attaches the engine so apply/reconcile can persist through
// the shopify mutations. Tests leave this nil and pin the Decision.
func (i *Integration) SetEngine(engine memql.IntegrationEngineAccess) {
	if i != nil {
		i.engine = engine
	}
}
