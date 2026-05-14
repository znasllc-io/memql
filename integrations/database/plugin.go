package database

import "github.com/visionarys-io/memql/component/memql"

// init self-registers the database integration as a plug-in. Always on:
// every node-type binary has the database integration available (even
// binaries that don't query a lot still need healthCheck/stats).
func init() {
	memql.RegisterPlugin("database", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewDatabaseIntegration(pctx.BunDB), nil
	})
}
