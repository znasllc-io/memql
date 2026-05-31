package openairealtime

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the OpenAI Realtime integration as a plug-in. The
// OpenAI key is resolved lazily at request time from
// v1:platform:globalSecret, so the factory returns successfully even on a
// fresh install -- the "key not configured" surface error fires only when a
// DSL caller invokes realtimeCreateClientSecret.
func init() {
	memql.RegisterPlugin("openairealtime", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(pctx.ResolveSystemSecret, pctx.Logger), nil
	})
}
