package shopify

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// config.go -- the env seed, and only the env seed.
//
// A single store used to be configured entirely in the environment, which a
// multi-store connector cannot live with: a store is added by an operator at
// runtime and each one has its own webhook secret, so the credentials belong
// on a row. These variables survive as a SEED -- they create the first store
// row on a cluster that has none and are then never read again.
//
// The distinction matters operationally. Editing MEMQL_SHOPIFY_STORE_DOMAIN
// on a cluster that already has a store row changes nothing, and that is
// correct rather than a bug: the row is the configuration, and an env var
// that silently overrode it would make the portal's view of a store a lie.

const (
	envStoreDomain    = "MEMQL_SHOPIFY_STORE_DOMAIN"
	envAdminToken     = "MEMQL_SHOPIFY_ADMIN_TOKEN"
	envStorefrontTok  = "MEMQL_SHOPIFY_STOREFRONT_TOKEN"
	envWebhookSecret  = "MEMQL_SHOPIFY_WEBHOOK_SECRET"
	envAPIVersion     = "MEMQL_SHOPIFY_API_VERSION"
	envAppClientID    = "MEMQL_SHOPIFY_APP_CLIENT_ID"
	envProtectedLevel = "MEMQL_SHOPIFY_PROTECTED_DATA_LEVEL"
)

// SeedConfig is the first store, as the environment describes it.
type SeedConfig struct {
	StoreDomain     string
	AdminToken      string
	StorefrontToken string
	WebhookSecret   string
	APIVersion      string
	AppClientID     string
	ProtectedLevel  string
}

// SeedConfigFromEnv reads the seed slots.
func SeedConfigFromEnv() SeedConfig {
	version := strings.TrimSpace(os.Getenv(envAPIVersion))
	if version == "" {
		version = generated.APIVersion
	}
	level := strings.TrimSpace(os.Getenv(envProtectedLevel))
	if level == "" {
		level = ProtectedNone
	}
	return SeedConfig{
		StoreDomain:     strings.TrimSpace(os.Getenv(envStoreDomain)),
		AdminToken:      strings.TrimSpace(os.Getenv(envAdminToken)),
		StorefrontToken: strings.TrimSpace(os.Getenv(envStorefrontTok)),
		WebhookSecret:   strings.TrimSpace(os.Getenv(envWebhookSecret)),
		APIVersion:      version,
		AppClientID:     strings.TrimSpace(os.Getenv(envAppClientID)),
		ProtectedLevel:  level,
	}
}

// Seedable reports whether the environment describes a store well enough to
// create one. A domain with no Admin token is not seedable: the row would
// exist, look configured in the portal, and fail every call.
func (c SeedConfig) Seedable() bool {
	return c.StoreDomain != "" && c.AdminToken != ""
}

// storeIDPattern bounds a derived store id to what an inbound source segment
// and an env-var suffix both accept. It is the same pattern
// component/inbound's sourceNamePattern enforces, one prefix shorter, because
// the composed name is "shopify-<id>".
var storeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,55}$`)

// StoreIDForDomain derives a store id from a myshopify domain.
//
// "acme-widgets.myshopify.com" becomes "acme-widgets". The id is part of the
// webhook URL Shopify is told to deliver to, so it has to be a legal path
// segment -- and it is stable for the life of the store, because changing it
// would change that URL and orphan every subscription.
func StoreIDForDomain(domain string) (string, error) {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimSuffix(d, ".myshopify.com")
	d = strings.TrimSuffix(d, "/")
	if i := strings.Index(d, "."); i > 0 {
		d = d[:i]
	}
	if !storeIDPattern.MatchString(d) {
		return "", fmt.Errorf("shopify: %q does not derive a usable store id (want %s)", domain, storeIDPattern)
	}
	return d, nil
}

// SeedStoreFromEnv creates the first store row when the environment describes
// one and no store exists.
//
// IT NEVER OVERWRITES. A cluster with any store row is already configured,
// and a boot-time write that reconciled the row against the environment would
// undo an operator's portal edit on every restart -- silently, and only on
// the nodes carrying the variables.
func SeedStoreFromEnv(ctx context.Context, engine memql.IntegrationEngineAccess, registry *StoreRegistry, cfg SeedConfig) (string, error) {
	if !cfg.Seedable() {
		return "", nil
	}
	existing, err := registry.Stores(ctx)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return "", nil
	}
	// Normalised HERE as well as at request time, so the row is written in
	// the one form every request path accepts -- and so a mistyped variable
	// is a refusal at boot rather than a store that looks configured in the
	// portal and fails every call it is asked to make.
	domain, err := NormalizeShopDomain(cfg.StoreDomain)
	if err != nil {
		return "", err
	}
	storeID, err := StoreIDForDomain(domain)
	if err != nil {
		return "", err
	}
	actorCtx := connectorContext(ctx)

	adminRef, err := seedSecret(actorCtx, engine, "SHOPIFY_"+strings.ToUpper(storeID)+"_ADMIN_TOKEN", cfg.AdminToken)
	if err != nil {
		return "", err
	}
	storefrontRef, err := seedSecret(actorCtx, engine, "SHOPIFY_"+strings.ToUpper(storeID)+"_STOREFRONT_TOKEN", cfg.StorefrontToken)
	if err != nil {
		return "", err
	}
	webhookRef, err := seedSecret(actorCtx, engine, "SHOPIFY_"+strings.ToUpper(storeID)+"_WEBHOOK_SECRET", cfg.WebhookSecret)
	if err != nil {
		return "", err
	}

	call := renderCall("createStore", map[string]any{
		"storeId":            storeID,
		"domain":             domain,
		"name":               domain,
		"appClientId":        cfg.AppClientID,
		"adminTokenRef":      adminRef,
		"storefrontTokenRef": storefrontRef,
		"webhookSecretRef":   webhookRef,
		"apiVersion":         cfg.APIVersion,
		"protectedDataLevel": cfg.ProtectedLevel,
	})
	if _, err := engine.Execute(actorCtx, call); err != nil {
		return "", fmt.Errorf("shopify: seed store: %w", err)
	}
	registry.Invalidate()
	return storeID, nil
}

// seedSecret writes a credential into a globalSecret row and returns its
// name. The token goes into the graph SEALED -- the row holds ciphertext and
// a four-character fingerprint, and the store row holds only this name.
func seedSecret(ctx context.Context, engine memql.IntegrationEngineAccess, name, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	ciphertext, fingerprint, err := secret.Encrypt(value)
	if err != nil {
		return "", fmt.Errorf("shopify: seal %s: %w", name, err)
	}
	call := renderCall("setGlobalSecret", map[string]any{
		"id":             "sec-" + strings.ToLower(strings.ReplaceAll(name, "_", "-")),
		"name":           name,
		"encryptedValue": ciphertext,
		"fingerprint":    fingerprint,
		"kind":           "vendor_api_key",
		"description":    "Seeded from the environment at first boot by the Shopify connector.",
		"addedBy":        "system:connector:" + ConnectorName,
	})
	if _, err := engine.Execute(ctx, call); err != nil {
		return "", fmt.Errorf("shopify: seed secret %s: %w", name, err)
	}
	return name, nil
}
