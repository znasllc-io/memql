package shopify

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the Shopify integration. Opt-out when the store
// domain or a Storefront/Admin token is missing — Shopify is optional.
func init() {
	memql.RegisterPlugin("shopify", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		cfg := ConfigFromEnv()
		if !cfg.Configured() {
			return nil, nil
		}
		return NewIntegration(NewClient(cfg)), nil
	})
}
