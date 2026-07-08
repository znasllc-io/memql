package chat

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the chat integration as a plug-in. Always on:
// every node-type binary that handles agent dispatch carries this so
// the recentChat capability resolves consistently. This is a generic,
// product-agnostic engine capability (platform consolidation #2472 /
// Decision 2): the single-utterance-stream-per-space model is
// platform-level, so any product reads the same stream via
// integration.chat.recentChat with zero product Go.
func init() {
	memql.RegisterPlugin("chat", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.BunDB), nil
	})
}
