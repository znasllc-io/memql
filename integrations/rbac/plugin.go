package rbac

import "github.com/znasllc-io/memql/component/memql"

// init self-registers the rbac governance integration as an always-on
// plug-in. Anchored from app/plugins_core.go so every node-type binary has the
// governPrincipal / canCreatePrincipal builtins available -- relational RBAC
// governance (Epic 1, memql#2071) is product-agnostic and runs on every node
// (E1.6 enforces it server-side on the request path). The integration is pure
// (rank arithmetic only), so the factory needs nothing from the plugin
// context.
func init() {
	memql.RegisterPlugin("rbac", func(_ memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(), nil
	})
}
