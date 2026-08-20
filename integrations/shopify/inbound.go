package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
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

// SetEngine attaches the engine so apply/reconcile can persist through
// the shopify mutations. Tests leave this nil and pin the Decision.
func (i *Integration) SetEngine(engine memql.IntegrationEngineAccess) {
	if i != nil {
		i.engine = engine
	}
}
