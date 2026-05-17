package identity

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the identity integration as a plug-in. Always on.
func init() {
	memql.RegisterPlugin("identity", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIdentityIntegration(), nil
	})
}
