package workbench

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the workbench integration as a plug-in. The
// integration is light (no DB / no SI deps) and compiles into every
// node-type binary; only the agent build actually exercises it via
// the tool loop, but linking it everywhere keeps the plug-in anchor
// list in app/plugins_core.go simple.
func init() {
	memql.RegisterPlugin("workbench", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		integ := NewIntegration(pctx.Logger)
		// memql#722: inject the engine so a successful fs_write can
		// promote a v1:library:generatedOutput row. pctx.Engine is the
		// app's *MemQLEngine (the IntegrationEngineAccess surface) and is
		// present for every node-type binary, so this covers single-node
		// and cluster mode alike. Nil-safe inside SetEngine / the
		// promotion path.
		integ.SetEngine(pctx.Engine)
		return integ, nil
	})
}
