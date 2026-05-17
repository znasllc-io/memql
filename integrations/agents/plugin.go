package agents

import (
	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the agents integration via the plug-in path.
// Anchored from app/plugins_core.go (or equivalent) via a blank
// import so the init() runs at process start.
//
// Returns (nil, nil) when PluginContext.Agents is nil -- this lets
// node-type binaries that don't build with the registry (or test
// harnesses that construct a bare engine) skip registration cleanly
// instead of erroring. The DSL builtin will then fail at invocation
// time with a clear "agents integration not initialized" message
// rather than at load time.
func init() {
	memql.RegisterPlugin("agents", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		integration := New(pctx.Agents, pctx.Engine)
		if integration == nil {
			// Agents is nil -- skip registration. PluginContext.Agents is
			// only nil on engines that haven't called LoadUnifiedAgents,
			// which today is just unit-test fixtures.
			return nil, nil
		}
		return integration, nil
	})
}
