package timeutil

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the timeutil integration as a core plug-in.
// Always on: stateless, no external dependencies, every node-type
// binary that runs the daily-space automations needs it.
func init() {
	memql.RegisterPlugin("timeutil", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(), nil
	})
}
