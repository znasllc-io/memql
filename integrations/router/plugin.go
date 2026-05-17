package router

import (
	"github.com/znasllc-io/memql/component/memql"
)

func init() {
	memql.RegisterPlugin("router", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(pctx.Engine, pctx.Providers, pctx.Policies, pctx.Logger), nil
	})
}
