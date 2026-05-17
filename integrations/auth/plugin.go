package auth

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the auth integration as a plug-in. Always on: every
// node-type binary exposes auth.resolveUser / auth.checkPermission to the
// DSL.
func init() {
	memql.RegisterPlugin("auth", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewAuthIntegration(), nil
	})
}
