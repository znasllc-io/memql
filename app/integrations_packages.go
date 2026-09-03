package app

import (
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// integrations_packages.go hands the packages pipeline its build surface
// (epic memql#4900, task memql#4901).
//
// ===========================================================================
// WIRED HERE BECAUSE ONLY app/ CAN SEE BOTH
// ===========================================================================
// component/packages holds a one-method Engine on purpose -- it is what makes
// the D6 ordering law assertable with no cluster, no network and no object
// storage -- so it has no way to reach the integration registry and find the
// workbench. app/ does, after materializePlugins, and this is the seam.
//
// NO BUILD TAG. Every node type that can run a deploy needs it, and which node
// types those are is a deploy concern rather than a compile one: the bff serves
// the OS's packageDeploy, and an agent may run one from a plan. The workbench
// integration itself links into every binary for the same reason, and decides
// at RUNTIME whether the work runs here or on a peer.
//
// The FLEET builder is deliberately not wired here: it needs worker streams,
// which only the agent node holds, so it is wired in the agent's own
// integration file beside the worker integration it reads from.
func (a *App) wirePackageBuildSurface() {
	pkgs := a.lookupPackagesIntegration()
	if pkgs == nil {
		// Not an error and not a warning. The packages plug-in is anchored in
		// plugins_core.go and materializes everywhere, so its absence means a
		// build that deliberately left it out -- and a node with no packages
		// surface has no deploy to give a build surface to.
		return
	}
	wb := a.lookupWorkbenchIntegration()
	if wb == nil {
		a.Logger.Warn("packages: the workbench integration was not found, so this node has no build surface; " +
			"a package whose built output is not committed will be refused at the build stage")
		return
	}
	pkgs.SetWorkbench(wb)
	a.Logger.Info("packages: build surface bound to the workbench",
		"remote", workbenchRemoteEnabled())
}

// lookupPackagesIntegration recovers the materialized packages plug-in, or nil.
func (a *App) lookupPackagesIntegration() *packages.Integration {
	if a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName("packages")
	if provider == nil {
		return nil
	}
	integ, _ := provider.(*packages.Integration)
	return integ
}

// Keep the workbench import honest on builds where nothing else in this file
// names the package: the runner interface packages.SetWorkbench takes is
// satisfied by *workbench.Integration and by nothing else here.
var _ = func(w *workbench.Integration) any { return w }
