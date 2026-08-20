// Package shopifypack is the client-agnostic Shopify pack (memql#4138).
//
// It is a PACK, not core: dsl/shopify stays the thin index, and this
// package cannot shadow it. Registration is opt-in (Register, or the
// shopifypack build-tag init) the same way examples/referencepack loads.
//
// The pack is the portal projection: attach a shop, store secret
// NAMES (never token values), toggle sync. The storefront keeps its
// own cart URL. Inbound webhooks stay on the core Shopify plugin.
package shopifypack

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"embed"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// Domain is the DSL namespace this pack owns. Distinct from core "shopify".
const Domain = "shopifypack"

// ContractVersion is the Plugin SDK contract this pack was built against.
const ContractVersion = memql.PluginContractVersion

const integrationName = "shopifypack"

// Manifest secret names. The pack stores these strings, never the values.
const (
	StorefrontTokenSecret = "MEMQL_SHOPIFY_STOREFRONT_TOKEN"
	AdminTokenSecret      = "MEMQL_SHOPIFY_ADMIN_TOKEN"
	StoreDomainSecret     = "MEMQL_SHOPIFY_STORE_DOMAIN"
	APIVersionSecret      = "MEMQL_SHOPIFY_API_VERSION"
)

var (
	secretNameRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,127}$`)
	tokenPrefixes = []string{"shpat_", "shpss_", "shpca_", "shpua_", "shppa_"}
)

//go:embed all:dsl
var packFS embed.FS

// Tree returns the pack's embedded .memql subtree.
func Tree() fs.FS {
	sub, err := fs.Sub(packFS, "dsl")
	if err != nil {
		panic("shopifypack: embedded dsl tree missing: " + err.Error())
	}
	return sub
}

// ValidSecretRef refuses token values. A secret field holds an env-style
// name (MEMQL_SHOPIFY_STOREFRONT_TOKEN), never shpat_... or a blob.
func ValidSecretRef(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return fmt.Errorf("secret reference is empty")
	}
	lower := strings.ToLower(n)
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(lower, p) {
			return fmt.Errorf("secret reference looks like a token value; store the secret NAME (e.g. %s)", StorefrontTokenSecret)
		}
	}
	if !secretNameRe.MatchString(n) {
		return fmt.Errorf("secret reference must be an env-style name (MEMQL_SHOPIFY_...), not a token value")
	}
	return nil
}

// Provider is the pack's IntegrationProvider.
type Provider struct{}

func (p *Provider) IntegrationName() string { return integrationName }

func (p *Provider) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "attachShop",
			Description: "Attach a Shopify shop. Token fields must be secret names, never token values.",
			Handler:     p.attachShop,
			ArgsSchema: map[string]string{
				"shopId":                "string (required)",
				"storeDomain":           "string (required)",
				"storefrontTokenSecret": "string (required) - secret NAME",
				"adminTokenSecret":      "string (required) - secret NAME",
				"storeDomainSecret":     "string (optional) - secret NAME",
				"apiVersionSecret":      "string (optional) - secret NAME",
				"apiVersion":            "string (optional)",
				"syncEnabled":           "bool (optional, default true)",
			},
		},
		{
			Name:        "setShopSecrets",
			Description: "Replace secret references. Names only; token values are refused.",
			Handler:     p.setShopSecrets,
			ArgsSchema: map[string]string{
				"shopId":                "string (required)",
				"storefrontTokenSecret": "string (required) - secret NAME",
				"adminTokenSecret":      "string (required) - secret NAME",
				"storeDomainSecret":     "string (optional)",
				"apiVersionSecret":      "string (optional)",
			},
		},
		{
			Name:        "setShopSync",
			Description: "Flip Shopify index sync on or off at runtime.",
			Handler:     p.setShopSync,
			ArgsSchema: map[string]string{
				"shopId":      "string (required)",
				"syncEnabled": "bool (required)",
			},
		},
	}
}

func (p *Provider) attachShop(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	shopID := strings.TrimSpace(argString(args, "shopId"))
	domain := strings.TrimSpace(argString(args, "storeDomain"))
	if shopID == "" || domain == "" {
		return nil, fmt.Errorf("shopifypack.attachShop: shopId and storeDomain are required")
	}
	storefront := argString(args, "storefrontTokenSecret")
	admin := argString(args, "adminTokenSecret")
	if err := ValidSecretRef(storefront); err != nil {
		return nil, fmt.Errorf("shopifypack.attachShop: storefrontTokenSecret: %w", err)
	}
	if err := ValidSecretRef(admin); err != nil {
		return nil, fmt.Errorf("shopifypack.attachShop: adminTokenSecret: %w", err)
	}
	if v := argString(args, "storeDomainSecret"); v != "" {
		if err := ValidSecretRef(v); err != nil {
			return nil, fmt.Errorf("shopifypack.attachShop: storeDomainSecret: %w", err)
		}
	}
	if v := argString(args, "apiVersionSecret"); v != "" {
		if err := ValidSecretRef(v); err != nil {
			return nil, fmt.Errorf("shopifypack.attachShop: apiVersionSecret: %w", err)
		}
	}
	sync := true
	if raw, ok := args["syncEnabled"]; ok {
		sync = truthy(raw)
	}
	return resultNode("shopAttached", map[string]any{
		"shopId":                shopID,
		"storeDomain":           domain,
		"storefrontTokenSecret": storefront,
		"adminTokenSecret":      admin,
		"storeDomainSecret":     argString(args, "storeDomainSecret"),
		"apiVersionSecret":      argString(args, "apiVersionSecret"),
		"apiVersion":            argString(args, "apiVersion"),
		"syncEnabled":           sync,
	})
}

func (p *Provider) setShopSecrets(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	shopID := strings.TrimSpace(argString(args, "shopId"))
	if shopID == "" {
		return nil, fmt.Errorf("shopifypack.setShopSecrets: shopId is required")
	}
	storefront := argString(args, "storefrontTokenSecret")
	admin := argString(args, "adminTokenSecret")
	if err := ValidSecretRef(storefront); err != nil {
		return nil, fmt.Errorf("shopifypack.setShopSecrets: storefrontTokenSecret: %w", err)
	}
	if err := ValidSecretRef(admin); err != nil {
		return nil, fmt.Errorf("shopifypack.setShopSecrets: adminTokenSecret: %w", err)
	}
	if v := argString(args, "storeDomainSecret"); v != "" {
		if err := ValidSecretRef(v); err != nil {
			return nil, fmt.Errorf("shopifypack.setShopSecrets: storeDomainSecret: %w", err)
		}
	}
	if v := argString(args, "apiVersionSecret"); v != "" {
		if err := ValidSecretRef(v); err != nil {
			return nil, fmt.Errorf("shopifypack.setShopSecrets: apiVersionSecret: %w", err)
		}
	}
	return resultNode("shopSecretsSet", map[string]any{
		"shopId":                shopID,
		"storefrontTokenSecret": storefront,
		"adminTokenSecret":      admin,
		"storeDomainSecret":     argString(args, "storeDomainSecret"),
		"apiVersionSecret":      argString(args, "apiVersionSecret"),
	})
}

func (p *Provider) setShopSync(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	shopID := strings.TrimSpace(argString(args, "shopId"))
	if shopID == "" {
		return nil, fmt.Errorf("shopifypack.setShopSync: shopId is required")
	}
	if _, ok := args["syncEnabled"]; !ok {
		return nil, fmt.Errorf("shopifypack.setShopSync: syncEnabled is required")
	}
	return resultNode("shopSyncSet", map[string]any{
		"shopId":      shopID,
		"syncEnabled": truthy(args["syncEnabled"]),
	})
}

func NewProvider(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
	_ = pctx
	return &Provider{}, nil
}

func Register(domain string) {
	memqldsl.RegisterTree(domain, Tree())
	memql.RegisterPluginForContract(domain, ContractVersion, NewProvider)
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	switch v := args[key].(type) {
	case string:
		return v
	default:
		return ""
	}
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	default:
		return false
	}
}

func resultNode(kind string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("shopifypack:%s", kind),
		Concept:   "integration:shopifypack:" + kind,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}
