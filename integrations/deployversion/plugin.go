package deployversion

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the deployversion integration as a core plug-in.
// Always on: stateless, no external dependencies. The deployment bundle's
// version-decision logic (dsl/deployment/logic.memql nextDeploymentVersion)
// calls suggestNextVersion, and the cockpit/runner surface that drives the
// deploy automations needs it available.
func init() {
	memql.RegisterPlugin("deployversion", func(_ memql.PluginContext) (memql.IntegrationProvider, error) {
		return NewIntegration(), nil
	})
}
