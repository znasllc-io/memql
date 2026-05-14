package liveavatar

import "github.com/visionarys-io/memql/component/memql"

// init self-registers the LiveAvatar integration as a plug-in. The
// API key + avatar/voice configuration are resolved lazily at request
// time from v1:platform:globalSecret and v1:platform:globalVariable, so the
// factory returns successfully even on a fresh install -- the actual
// "key not configured" surface error fires only when a DSL caller
// invokes one of the capabilities.
func init() {
	memql.RegisterPlugin("liveavatar", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(pctx.ResolveSystemSecret, pctx.ResolveSystemVariable, pctx.Logger), nil
	})
}
