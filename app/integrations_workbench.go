//go:build workbench

package app

// integrationsWorkbench registers integration providers for a
// workbench node. The workbench binary is intentionally light --
// it loads:
//
//   - core integrations (database, auth, identity, embedding,
//     storage, etc.) via the plug-in path,
//   - the workbench plug-in itself (anchored in plugins_core.go),
//
// and nothing else. No STT, no agent reply pipeline, no cognition
// integration -- those are agent / cognition / planner-node
// concerns. The workbench's only job is to receive
// WorkbenchForwardRequest messages on its NodeService.Stream and
// dispatch them to the local integration handler.
func (a *App) integrationsWorkbench() {
	a.integrationsCore()
	a.Logger.Info("workbench integration providers registered")
}
