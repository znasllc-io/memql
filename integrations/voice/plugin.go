package voice

import "github.com/visionarys-io/memql/component/memql"

func init() {
	memql.RegisterPlugin("voice", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(pctx.Logger), nil
	})
}
