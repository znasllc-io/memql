package liveknowledge

import (
	"github.com/znasllc-io/memql/component/memql"
	lk "github.com/znasllc-io/memql/component/memql/liveknowledge"
)

// init self-registers the liveknowledge integration as a plug-in.
//
// The plug-in pulls the engine from PluginContext and constructs the
// default DSL-callable surface:
//
//  1. A Dispatcher pinned to the engine.
//  2. A built-in MemqlConnector registered against the default
//     Registry singleton -- supports kind='memql' liveConnector rows
//     (queries against the local engine, the common case).
//  3. The integration wrapping the dispatcher.
//
// Other connector kinds (postgres / mysql / mssql / rest / graphql /
// custom) ship as their own plug-ins; each calls
// liveknowledge.DefaultRegistry().Register(...) at init() time to
// extend the supported kind set without modifying this plug-in.
func init() {
	memql.RegisterPlugin("liveknowledge", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		engineAccess := &engineAdapter{exec: pctx.Engine}

		// Register the built-in memql-kind connector.
		lk.DefaultRegistry().Register(lk.NewMemqlConnector(engineAccess))

		dispatcher := lk.NewDispatcher(engineAccess, lk.DefaultRegistry(), pctx.Logger)
		return New(dispatcher), nil
	})
}
