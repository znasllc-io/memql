//go:build cognition

package app

// integrationsCognition registers integration providers for a cognition node.
// Cognition nodes need: core (database, auth) + cognition integration.
// STT is on the voice node, not here.
func (a *App) integrationsCognition() {
	a.integrationsCore()
	a.setupCognitionIntegration()
	a.Logger.Info("cognition integration providers registered")
}
