package dailyspace

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the dailyspace integration as a core plug-in.
// Always on: the daily-space automations live in core DSL and need
// these capabilities on every node-type binary that runs cognition
// automations. The plug-in needs the engine handle to re-enter
// Execute (read user, write space), so the factory plucks it off
// PluginContext.
func init() {
	memql.RegisterPlugin("dailyspace", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Engine), nil
	})
}
