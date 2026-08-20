package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// Integration is the Shopify IntegrationProvider. First slice (#4136):
// read GID, handle, availableForSale. Thin index (#4137): inbound +
// scheduled reconcile upsert those three fields only. No tags,
// shipping, metafields, or checkout ownership.
type Integration struct {
	client *Client
	engine memql.IntegrationEngineAccess
}

// NewIntegration wraps a server-side Shopify client. client may be
// nil when Shopify is unconfigured -- apply/reconcile then no-op so
// inbound from other sources does not fail.
func NewIntegration(client *Client) *Integration {
	return &Integration{client: client}
}

func (i *Integration) IntegrationName() string { return "shopify" }

func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "fetchProduct",
			Description: "Read a Shopify product's GID, handle, and availableForSale from the Storefront or Admin API. Server-side only.",
			Handler:     i.handleFetchProduct,
			ArgsSchema: map[string]string{
				"id":     "string (optional) - Shopify product GID",
				"handle": "string (optional) - storefront handle",
			},
		},
		{
			Name:        "applyInboundProduct",
			Description: "Parse a staged inbound Shopify product webhook, fetch GID/handle/availableForSale, upsert only on a hit. A miss is not invented.",
			Handler:     i.handleApplyInboundProduct,
			ArgsSchema: map[string]string{
				"inboundRequestId": "string (optional) - staged inbound row id",
				"body":             "string (optional) - raw webhook body",
				"source":           "string (optional) - inbound source name",
				"topic":            "string (optional) - X-Shopify-Topic when the caller has it",
			},
		},
		{
			Name:        "reconcileProduct",
			Description: "Re-fetch one thin-index product. Upsert on a hit; retire on a miss. Never invents.",
			Handler:     i.handleReconcileProduct,
			ArgsSchema: map[string]string{
				"id":     "string (optional) - Shopify product GID",
				"handle": "string (optional) - storefront handle",
			},
		},
		{
			Name:        "reconcileIndex",
			Description: "Re-fetch present thin-index rows. Each known GID is re-fetched; nothing new is invented. No-op when unconfigured.",
			Handler:     i.handleReconcileIndex,
		},
	}
}

func (i *Integration) handleFetchProduct(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.client == nil {
		return nil, fmt.Errorf("shopify: not configured")
	}
	p, err := i.client.FetchProduct(ctx, asString(args["id"]), asString(args["handle"]))
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"id":               p.ID,
		"handle":           p.Handle,
		"availableForSale": p.AvailableForSale,
	})
	if err != nil {
		return nil, fmt.Errorf("shopify: marshal product: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        p.ID,
		Concept:   "integration:shopify:product",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

func (i *Integration) handleApplyInboundProduct(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.client == nil {
		return skipNode("shopify unconfigured")
	}
	source := asString(args["source"])
	topic := asString(args["topic"])
	body := []byte(asString(args["body"]))
	gid, handle, ok := ParseProductDelivery(source, topic, body)
	if !ok {
		return skipNode("not a shopify product delivery")
	}
	return i.applyFetched(ctx, gid, handle)
}

func (i *Integration) handleReconcileProduct(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.client == nil {
		return skipNode("shopify unconfigured")
	}
	id := asString(args["id"])
	handle := asString(args["handle"])
	if id == "" && handle == "" {
		return skipNode("id or handle is required")
	}
	return i.applyFetched(ctx, id, handle)
}

func (i *Integration) handleReconcileIndex(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i == nil || i.client == nil {
		return skipNode("shopify unconfigured")
	}
	if i.engine == nil {
		return skipNode("engine not attached")
	}
	res, err := i.engine.Execute(ctx, `shopifyProducts(present: true)`)
	if err != nil {
		return nil, fmt.Errorf("shopify: list index: %w", err)
	}
	ids := productIDsFromResult(res)
	var last []memorynodes.MemoryNode
	for _, id := range ids {
		nodes, err := i.applyFetched(ctx, id, "")
		if err != nil {
			return nil, err
		}
		last = nodes
	}
	if last == nil {
		return skipNode("empty index")
	}
	return last, nil
}

func productIDsFromResult(res *memql.ExecuteResult) []string {
	if res == nil || res.Bundle == nil {
		return nil
	}
	out := make([]string, 0, len(res.Bundle.Nodes))
	for _, n := range res.Bundle.Nodes {
		if n != nil && n.Id != "" {
			out = append(out, n.Id)
		}
	}
	return out
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
