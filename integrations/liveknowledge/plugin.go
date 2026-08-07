package liveknowledge

import (
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	lk "github.com/znasllc-io/memql/core/liveknowledge"
)

// newDefaultMemqlConnector builds the kind='memql' connector with the ONE
// correct MemQL string quoter.
//
// The quoter is a parameter of lk.NewMemqlConnector rather than something
// core/liveknowledge implements, because that package is an L0 leaf with zero
// in-repo imports (memql#3164) and so cannot reach langparser.QuoteString --
// the definition that lives beside the lexer whose escape set it targets.
// This package can, so the wiring happens here. Named rather than inlined so
// the regression guard exercises the same construction the plug-in registers;
// see memql_connector_control_byte_test.go and memql#3192.
func newDefaultMemqlConnector(engine lk.EngineAccess) *lk.MemqlConnector {
	return lk.NewMemqlConnector(engine, langparser.QuoteString)
}

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
		lk.DefaultRegistry().Register(newDefaultMemqlConnector(engineAccess))

		dispatcher := lk.NewDispatcher(engineAccess, lk.DefaultRegistry(), pctx.Logger)
		return New(dispatcher), nil
	})
}
