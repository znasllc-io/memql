//go:build !agent && !planner

package app

import (
	"net/http"

	"github.com/znasllc-io/memql/component/polyphon"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/polyphonws"
)

// wirePolyphonEndpoints registers the Polyphon multi-agent voice HTTP endpoints
// (room-token, status, utterance) and the LiveKit room provider. Build tag is
// `!agent && !planner` so this compiles into bff, voice, cognition, default,
// and identity binaries; each transport_*.go file decides whether to call it.
// BFF + default + voice currently call it; cognition + identity don't (they
// have no browsers connecting directly).
//
// The Go Bridge Agent that historically owned the ASR -> ScoreEngine -> LLM
// -> TTS pipeline was retired in Initiative C Phase 11, and the Python
// voice-agent (LiveKit Agents 1.5) that briefly succeeded it was retired in
// epic #449. The Go voice-agent (integrations/voice/agent) is now the sole
// voice participant for the Assistant in LiveKit rooms; it speaks the
// VoiceAgent* gRPC contract on MemqlService.Stream.
func (a *App) wirePolyphonEndpoints() {
	cfg := polyphon.ConfigFromEnv()

	// HTTP endpoint registration
	var roomProvider polyphon.RoomProvider
	if cfg.LiveKitConfigured() {
		roomProvider = polyphon.NewLocalRoomProvider(cfg)
		a.Logger.Info("polyphon: local room provider enabled (LiveKit token generation)")
	}

	var polyphonScoreEng *polyphon.ScoreEngine
	if a.polyphonScoreEngine != nil {
		polyphonScoreEng = a.polyphonScoreEngine.(*polyphon.ScoreEngine)
	}

	handler := polyphonws.NewHandler(polyphonScoreEng, roomProvider, "")

	// Wire Polyphon dependencies to gRPC server.
	if a.grpcServer != nil {
		a.grpcServer.SetScoreEngine(polyphonScoreEng)
		a.grpcServer.SetRoomProvider(roomProvider)
	}

	for _, path := range server.PolyphonRoomTokenPaths() {
		a.handleRoute("POST "+path, http.HandlerFunc(handler.ServeRoomToken))
	}
	for _, path := range server.PolyphonStatusPaths() {
		a.handleRoute("GET "+path, http.HandlerFunc(handler.ServeStatus))
	}
	// /polyphon/utterance and /polyphon/preload used to register here too.
	// Both were unauthenticated (server.PublicPaths()) and existed only for
	// the retired Bridge Agent; removed in memql#3531. The authenticated
	// equivalent is PolyphonUtteranceMsg on MemqlService.Stream.

	if cfg.LiveKitConfigured() {
		a.Logger.Info("polyphon endpoints registered (LiveKit configured)",
			"roomToken", server.PolyphonRoomTokenPaths(),
			"status", server.PolyphonStatusPaths(),
			"livekitURL", cfg.LiveKitURL,
		)
	} else {
		a.Logger.Info("polyphon endpoints registered (LiveKit not configured, room tokens unavailable)")
	}
}
