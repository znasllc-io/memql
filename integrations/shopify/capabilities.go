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
// read GID, handle, availableForSale. No tags, shipping, metafields,
// or checkout ownership.
type Integration struct {
	client *Client
}

// NewIntegration wraps a server-side Shopify client.
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

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
