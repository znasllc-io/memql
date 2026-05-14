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
	a.Logger.Info("core integration providers registered")
}
