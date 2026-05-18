//go:build bff

package app

// transportBFF sets up transport for a BFF node: gRPC (MemqlService.Stream),
// the HTTP server (for ws upgrade + the auth / attachment exceptions),
// and any BFF-specific domain endpoints added by product branches.
// SI operations (chat, speech, transcribe, agent/space/group suggest)
// live on MemqlService.Stream via SIChatMsg / SISpeechMsg / SITranscribeMsg /
// SISuggestMsg and are proxied across nodes by SIForwardRouter.
//
// Polyphon: the BFF also wires the LiveKit room provider so browsers
// can fetch a room token (PolyphonRoomTokenMsg) directly from the
// BFF gRPC stream they're already connected to. The score engine
// itself runs on the cognition node; the BFF just mints LiveKit
// tokens. The bridge agent is a separate container that joins the
// room independently.
func (a *App) transportBFF() {
	a.transportBase()
	a.wirePolyphonEndpoints()
	a.createHTTPServer()
}
