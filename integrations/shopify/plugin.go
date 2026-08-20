package shopify

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the Shopify integration.
//
// #4136 opted out entirely when the store domain or a token was
// missing (return nil, nil). #4137's inbound automation always
// loads, so a missing executor would fail every inbound row from
// every source. We still skip FetchProduct / persist when
// unconfigured -- apply/reconcile no-op instead of inventing -- but
// the plug-in stays registered so the builtins exist.
func init() {
	memql.RegisterPlugin("shopify", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		cfg := ConfigFromEnv()
		var client *Client
		if cfg.Configured() {
			client = NewClient(cfg)
		}
		i := NewIntegration(client)
		i.SetEngine(pctx.Engine)
		return i, nil
	})
}
