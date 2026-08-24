package app

// integrationsCore materializes every integration plug-in that has
// self-registered via memql.RegisterPlugin. The set of registrations is
// controlled by the blank imports in app/plugins_core.go (always-on) and
// build-tag-gated anchor files alongside it.
//
// Integrations with dependencies that don't fit the PluginContext surface
// (cognition, agent, stt) are still wired explicitly in the corresponding
// build-tagged app/integrations_*.go files.
func (a *App) integrationsCore() {
	a.materializePlugins()
	// The Shopify connector's boot work -- seeding the first store from the
	// environment and reconciling every store's webhook subscriptions --
	// runs after registration and in the background. See
	// app/integrations_shopify.go for why it cannot be in the factory.
	a.registerShopifyConnector()
	a.Logger.Info("core integration providers registered")
}
