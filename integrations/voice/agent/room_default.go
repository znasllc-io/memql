//go:build !voice

package agent

import "log/slog"

// room_default.go is the CGO-free counterpart to room_voice.go. In the
// default build (no `-tags voice`) the LiveKit server SDK and its CGO
// libopus/soxr media-sdk are deliberately NOT imported, so there is no real
// room joiner. NewRoomJoiner returns nil; Session.Run treats a nil joiner as
// "run the gRPC handshake and return" (used by the default build + the
// CGO-free tests). The real LiveKit join lives in room_voice.go and is only
// compiled into the voice node binary.
func NewRoomJoiner(_ Config, _ *Client, _ *slog.Logger) RoomJoiner {
	return nil
}
