package harnesstrace

import (
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/memql"
)

// init self-registers the harnessTrace integration as an always-on
// plug-in. Anchored from app/plugins_core.go (next to harnessRecall) so
// every node-type binary with a database exposes harnessTrace -- the
// trace assembler is product-agnostic and reads the harness plan/step/
// observation stream straight from the graph.
//
// Returns (nil, nil) to opt out cleanly when the binary has no DB
// handle (the trace builtin then resolves to "no handler" rather than
// failing startup), mirroring harnessrecall.
func init() {
	memql.RegisterPlugin("harnessTrace", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		if pctx.BunDB == nil {
			// No database getter on this node-type binary; the trace
			// reader has nothing to read. Opt out cleanly.
			return nil, nil
		}
		integ := New(pctx.Logger)
		integ.SetDBGetter(func() *bun.DB {
			return pctx.BunDB()
		})
		return integ, nil
	})
}
