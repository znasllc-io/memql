package shopify

import (
	"log/slog"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// init self-registers the Shopify integration.
//
// #4136 opted out entirely when the store domain or a token was
// missing (return nil, nil). #4137's inbound automation always
// loads, so a missing executor would fail every inbound row from
// every source. We still skip FetchProduct / persist when
// unconfigured -- apply/reconcile no-op instead of inventing -- but
// the plug-in stays registered so the builtins exist.
//
// The connector NAME is declared in connector.go's init() rather than
// here, because the engine's boot check resolves @origin("shopify")
// before integrations are wired at all. This factory BINDS the
// implementation, which is the later half of the same registration.
func init() {
	memql.RegisterPlugin("shopify", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		cfg := ConfigFromEnv()
		var client *Client
		if cfg.Configured() {
			client = NewClient(cfg)
		}
		i := NewIntegration(client)
		i.SetEngine(pctx.Engine)

		// Bind the contract surface. A bind failure is LOGGED, not
		// returned: the only ways it can fail are a name nothing
		// declared (impossible here -- connector.go's init() declares
		// it) and a second bind under one name, which happens when a
		// process materializes plug-ins twice. Neither is a reason to
		// take down a node whose builtins and inbound automation are
		// otherwise fine, and the mirror write guard does not depend on
		// the binding -- it matches the ACTOR's name against the
		// concept's declaration, so writes stay correctly gated whether
		// or not an implementation is bound here.
		if err := memqlsync.Bind(i.Connector()); err != nil {
			logger := pctx.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("shopify connector was not bound; inbound and reconcile still run through the builtins",
				"component", "integrations.shopify", "error", err)
		}
		return i, nil
	})
}
