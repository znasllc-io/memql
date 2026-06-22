//go:build voice

package app

import (
	"github.com/znasllc-io/memql/integrations/stt"
)

// transportVoiceEndpoints sets up transport for a voice node.
// Includes: gRPC, WebSocket, Polyphon voice, STT. The legacy
// HTTP AI endpoints (/si/speech, /si/transcribe, /si/chat, /si/*/suggest)
// have been retired -- callers use MemqlService.Stream with AiSpeechMsg /
// AiTranscribeMsg / AiChatMsg / AiSuggestMsg instead. Cross-node AI
// requests ride through BFF's AiForwardRouter.
func (a *App) transportVoiceEndpoints() {
	a.transportBase()

	// STT on gRPC server
	if a.sttProvider != nil {
		a.grpcServer.SetSTTProvider(a.sttProvider.(stt.StreamingProvider))
	}

	// Audio WebSocket (browser STT + Read-Aloud TTS). The voice node is
	// the canonical /memql/audio terminus -- nginx proxies it here. Shared
	// with the agent node; see transport_audio.go.
	a.setupAudioWebsocket()

	// Polyphon multi-agent voice
	a.wirePolyphonEndpoints()

	a.createHTTPServer()
}
