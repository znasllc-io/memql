package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Product is the thin index slice #4136 / #4137 need. Merchandising
// stays on Shopify; checkout stays cart.checkoutUrl.
type Product struct {
	ID               string `json:"id"`
	Handle           string `json:"handle"`
	AvailableForSale bool   `json:"availableForSale"`
}

// Client talks to Shopify Storefront and/or Admin GraphQL from the
// server. Tokens never appear on a browser-reachable surface.
type Client struct {
	cfg  Config
	http *http.Client
}

// NewClient builds a server-side Shopify client. Callers must not
// embed this in a client bundle.
func NewClient(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

const productQuery = `query Product($id: ID, $handle: String) {
  product(id: $id, handle: $handle) {
    id
    handle
    availableForSale
  }
}`

const adminProductQuery = `query Product($id: ID!) {
  product(id: $id) {
    id
    handle
    availableForSale
  }
}`

// FetchProduct reads GID, handle, and availableForSale by Storefront
// GID or handle. If only an Admin token is configured, Admin GraphQL
// is used (GID required). Checkout is not implemented.
func (c *Client) FetchProduct(ctx context.Context, id, handle string) (Product, error) {
	if c == nil {
		return Product{}, fmt.Errorf("shopify: client is nil")
	}
	id = strings.TrimSpace(id)
	handle = strings.TrimSpace(handle)
	if id == "" && handle == "" {
		return Product{}, fmt.Errorf("shopify: id or handle is required")
	}
	if c.cfg.StorefrontToken != "" {
		return c.fetchStorefront(ctx, id, handle)
	}
	if c.cfg.AdminToken != "" {
		if id == "" {
			return Product{}, fmt.Errorf("shopify: admin product read requires a GID")
		}
		return c.fetchAdmin(ctx, id)
	}
	return Product{}, fmt.Errorf("shopify: no storefront or admin token configured")
}

func (c *Client) fetchStorefront(ctx context.Context, id, handle string) (Product, error) {
	url := c.cfg.StorefrontBaseURL
	if url == "" {
		if c.cfg.StoreDomain == "" {
			return Product{}, fmt.Errorf("shopify: store domain is empty")
		}
		url = fmt.Sprintf("https://%s/api/%s/graphql.json", strings.TrimSuffix(c.cfg.StoreDomain, "/"), c.cfg.APIVersion)
	}
	vars := map[string]any{}
	if id != "" {
		vars["id"] = id
	}
	if handle != "" {
		vars["handle"] = handle
	}
	body, err := c.postGraphQL(ctx, url, c.cfg.StorefrontToken, "X-Shopify-Storefront-Access-Token", productQuery, vars)
	if err != nil {
		return Product{}, err
	}
	return decodeProduct(body, "storefront")
}

func (c *Client) fetchAdmin(ctx context.Context, id string) (Product, error) {
	url := c.cfg.AdminBaseURL
	if url == "" {
		if c.cfg.StoreDomain == "" {
			return Product{}, fmt.Errorf("shopify: store domain is empty")
		}
		url = fmt.Sprintf("https://%s/admin/api/%s/graphql.json", strings.TrimSuffix(c.cfg.StoreDomain, "/"), c.cfg.APIVersion)
	}
	body, err := c.postGraphQL(ctx, url, c.cfg.AdminToken, "X-Shopify-Access-Token", adminProductQuery, map[string]any{"id": id})
	if err != nil {
		return Product{}, err
	}
	return decodeProduct(body, "admin")
}

func (c *Client) postGraphQL(ctx context.Context, url, token, tokenHeader, query string, variables map[string]any) ([]byte, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("shopify: encode graphql: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("shopify: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tokenHeader, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("shopify: graphql: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("shopify: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("shopify: graphql status %d", resp.StatusCode)
	}
	return raw, nil
}

type gqlEnvelope struct {
	Data struct {
		Product *struct {
			ID               string `json:"id"`
			Handle           string `json:"handle"`
			AvailableForSale bool   `json:"availableForSale"`
		} `json:"product"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func decodeProduct(raw []byte, surface string) (Product, error) {
	var env gqlEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Product{}, fmt.Errorf("shopify: decode %s: %w", surface, err)
	}
	if len(env.Errors) > 0 {
		return Product{}, fmt.Errorf("shopify: %s graphql: %s", surface, env.Errors[0].Message)
	}
	if env.Data.Product == nil {
		return Product{}, fmt.Errorf("shopify: product not found")
	}
	p := env.Data.Product
	if p.ID == "" || p.Handle == "" {
		return Product{}, fmt.Errorf("shopify: product missing id or handle")
	}
	return Product{ID: p.ID, Handle: p.Handle, AvailableForSale: p.AvailableForSale}, nil
}
