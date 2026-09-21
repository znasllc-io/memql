package app

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/packages/githubapp"
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

// githubAppReadTimeout bounds one read of the six rows. The client asks for the
// app's configuration from methods that carry no context of their own
// (Configured, InstallURL), so the read gets one here -- and a store that does
// not answer in this long is answered as "no app right now", which the next
// call corrects.
const githubAppReadTimeout = 5 * time.Second

// wirePackageGitHubApp tells the packages pipeline where the cluster's GitHub
// App comes from (design record 2026-09-20-github-app-setup, D2).
//
// WIRED HERE FOR THE REASON THE BUILD SURFACE IS. component/packages reads five
// of the app's six values and component/identity/githubconnect owns the rule
// for where all six come from -- the environment, or the rows a cluster
// owner's registration wrote, never a mix. Only app/ can see both, and one
// rule with two readers is the point: a fetch that resolved the app differently
// from the Connect button that granted it would mint under an app the grant
// was never made for.
//
// NO BUILD TAG, like its neighbour: every node type that can fetch, poll or
// probe needs to see a registration without a restart.
func (a *App) wirePackageGitHubApp() {
	pkgs := a.lookupPackagesIntegration()
	if pkgs == nil || a.engine == nil {
		return
	}
	resolver := &githubconnect.Resolver{Rows: githubconnect.RowReader{
		Variable: a.engine.ResolveSystemVariable,
		Secret:   a.engine.ResolveSystemSecret,
	}}
	pkgs.SetGitHubAppSource(func() githubapp.Config {
		ctx, cancel := context.WithTimeout(context.Background(), githubAppReadTimeout)
		defer cancel()
		cfg, _ := resolver.Current(ctx)
		// FIVE OF THE SIX. The webhook secret is deliberately not this
		// package's to hold: nothing in it verifies a delivery, and a second
		// reader of a signing secret is a second place it can be logged
		// (githubapp/config.go).
		return githubapp.Config{
			AppId:         cfg.AppID,
			Slug:          cfg.AppSlug,
			ClientId:      cfg.ClientID,
			ClientSecret:  cfg.ClientSecret,
			PrivateKeyB64: cfg.PrivateKeyB64,
		}
	})
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
