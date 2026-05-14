//go:build voice

package app

import "log/slog"

// Build constructs service dependencies for a voice node.
// Voice nodes handle the audio I/O pipeline: ASR, TTS, and LiveKit room
// management. They do NOT run the Polyphon scoring engine or cognition
// integration -- those live on the cognition node. Voice and cognition
// communicate via events and the shared database.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsVoice()
	a.transportVoiceEndpoints()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
