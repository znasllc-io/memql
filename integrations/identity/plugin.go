package identity

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the identity integration as a plug-in. Always on.
func init() {
	memql.RegisterPlugin("identity", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// The engine + DB are handed over so ownership transfer (memql#4838)
		// can enumerate declared concepts and write back through the engine.
		// A node without them still gets every delegation capability.
		return NewIdentityIntegrationWithEngine(pctx.Engine, pctx.BunDB), nil
	})
}
