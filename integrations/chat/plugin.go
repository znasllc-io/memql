package chat

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the chat integration as a plug-in. Always on:
// every node-type binary that handles agent dispatch carries this so
// the recentChat tool resolves consistently. The integration is a
// core memql capability (was previously the BFF copresent.conversation
// handler) because the single-chat architecture is platform-level --
// every frontend (CoPresent, cockpit, future apps) reads the same
// stream.
func init() {
	memql.RegisterPlugin("chat", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.BunDB), nil
	})
}
