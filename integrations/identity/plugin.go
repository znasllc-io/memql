package identity

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the identity integration as a plug-in. Always on.
func init() {
	memql.RegisterPlugin("identity", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		// The engine + DB are handed over so ownership transfer (memql#4838)
		// can enumerate declared concepts and write back through the engine,
		// and GitHub Connect (memql#4913) can write its state row. The logger
		// goes with them because githubConnectBegin answers a typed reason
		// rather than an error, and a reason a client renders is not a record
		// an operator can read. A node without any of the three still gets
		// every delegation capability.
		return NewIdentityIntegrationWithEngine(pctx.Engine, pctx.BunDB, pctx.Logger), nil
	})
}
