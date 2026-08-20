package shopify

import (
	"os"
	"strings"
)

const (
	defaultAPIVersion = "2025-01"
	envStoreDomain    = "MEMQL_SHOPIFY_STORE_DOMAIN"
	envStorefrontTok  = "MEMQL_SHOPIFY_STOREFRONT_TOKEN"
	envAdminTok       = "MEMQL_SHOPIFY_ADMIN_TOKEN"
	envAPIVersion     = "MEMQL_SHOPIFY_API_VERSION"
)

// Config is the server-side Shopify credentials for this process.
// Tokens never leave this package except as HTTP request headers.
type Config struct {
	StoreDomain       string
	StorefrontToken   string
	AdminToken        string
	APIVersion        string
	StorefrontBaseURL string // override for tests
	AdminBaseURL      string // override for tests
}

// ConfigFromEnv reads the MEMQL_SHOPIFY_* slots. Empty tokens mean the
// plug-in should opt out — Shopify is optional.
func ConfigFromEnv() Config {
	version := strings.TrimSpace(os.Getenv(envAPIVersion))
	if version == "" {
		version = defaultAPIVersion
	}
	return Config{
		StoreDomain:     strings.TrimSpace(os.Getenv(envStoreDomain)),
		StorefrontToken: strings.TrimSpace(os.Getenv(envStorefrontTok)),
		AdminToken:      strings.TrimSpace(os.Getenv(envAdminTok)),
		APIVersion:      version,
	}
}

// Configured reports whether we can make a Storefront (or Admin) product read.
func (c Config) Configured() bool {
	return c.StoreDomain != "" && (c.StorefrontToken != "" || c.AdminToken != "")
}
