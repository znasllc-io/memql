package router

import (
	"sync/atomic"

	"github.com/znasllc-io/memql/component/memql"
)

// lastRouterIntegration is how app/ reaches the instance the plug-in factory
// built, to install the evidence fold's rule renderer on it.
//
// It is a package-level handle rather than a lookup because the plug-in system
// hands the instance to the ENGINE and gives the caller no way back to it, and
// a second registration path purely to retrieve one would be a bigger seam than
// this. Last-wins is correct: exactly one router integration materializes per
// process, and a test that builds a second is describing the second.
var lastRouterIntegration atomic.Pointer[Integration]

// Materialized returns the router integration this process built, or nil.
func Materialized() *Integration { return lastRouterIntegration.Load() }

func init() {
	memql.RegisterPlugin("router", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		i := New(pctx.Engine, pctx.Providers, pctx.Policies, pctx.Logger)
		// The rule renderer is installed from app/ (SetRuleRenderer). It cannot
		// come through PluginContext: this module requires the root module at a
		// published version, and the renderer (component/routingrules) lives in
		// the root module and is newer than any published version of it.
		lastRouterIntegration.Store(i)
		return i, nil
	})
}
