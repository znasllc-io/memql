package workbench

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the workbench integration as a plug-in. The
// integration is light (no DB / no SI deps) and compiles into every
// node-type binary; only the agent build actually exercises it via
// the tool loop, but linking it everywhere keeps the plug-in anchor
// list in app/plugins_core.go simple.
func init() {
	memql.RegisterPlugin("workbench", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(pctx.Logger), nil
	})
}
