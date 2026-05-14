//go:build voice

package app

// integrationsVoice registers integration providers for a voice node.
// Voice nodes need: core (database, auth) + STT provider.
// They do NOT need the cognition integration (scoring, response generation).
func (a *App) integrationsVoice() {
	a.integrationsCore()
	a.selectSTTProvider()
	a.Logger.Info("voice integration providers registered")
}
