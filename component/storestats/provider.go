package storestats

import (
	"github.com/znasllc-io/memql/component/memql"
)

// The DSL-callable half, declared in dsl/shopify/overlay/builtins.memql and
// executed here. `builtin storeStatisticsRollup` is the walk that keeps every
// store's statistics (rollup.go), which only its own automation's maintenance
// principal may run.
//
// # Why it is registered on EVERY node type
//
// The builtin is declared in the shopify overlay, a tree every binary loads,
// and a capability present in the DSL and absent from the registry is a
// boot-time resolution failure. So the provider registers everywhere, from
// app/plugins_core.go, as component/emailstats does; the cron leader, wherever
// it is, runs the rollup.

// integrationName is the provider's name. Spelled as a string literal in
// RegisterPlugin below as well, because the taxonomy gate scans source for the
// literal; TestTheProviderOffersWhatItsBuiltinsName holds the two together.
const integrationName = "storeStats"

func init() {
	memql.RegisterPlugin("storeStats", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Engine, pctx.BunDB, pctx.Logger), nil
	})
}
