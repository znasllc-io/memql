//go:build agent || voice

package app

import (
	"net/http"

	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/audiows"
	"github.com/znasllc-io/memql/integrations/stt"
)

// setupAudioWebsocket registers the /memql/audio browser WebSocket
// handler (in-space STT + Read-Aloud TTS). It is built into the voice
// and agent nodes -- the two node types that carry an STT provider and
// the audio handler per docs/public/build/build-tags.md. The voice node is the
// canonical browser audio terminus (nginx proxies /memql/audio there);
// the agent keeps it for agent-originated audio.
//
// Auth rides the shared verifier HTTP middleware: the handler reads the
// identity off the request context (populated from the Authorization
// header or the bearer WebSocket subprotocol the SDK sends on the upgrade,
// with the deprecated ?bearer_token= query still accepted), so
// no token extraction lives here.
//
// No-op when no STT provider was selected (e.g. missing provider keys),
// matching the previous inline guard.
func (a *App) setupAudioWebsocket() {
	if a.sttProvider == nil {
		return
	}

	sttProv := a.sttProvider.(stt.StreamingProvider)
	ttsProvider := a.engine.TTSProvider()

	audioHandler, err := audiows.New(audiows.Options{
		Logger:      a.Logger,
		STTProvider: sttProv,
		TTSProvider: ttsProvider,
		Engine:      &AudioEngineAdapter{Engine: a.engine},
	})
	if err != nil {
		a.fatal("failed to initialize audio websocket handler", "error", err)
	}

	for _, path := range server.AudioWebsocketPaths() {
		a.mux.Handle("GET "+path, http.HandlerFunc(audioHandler.ServeHTTP))
	}
	a.Logger.Info("audio websocket enabled",
		"paths", server.AudioWebsocketPaths(),
		"sttProvider", sttProv.Name(),
	)
}
