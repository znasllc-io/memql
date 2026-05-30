package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// session.go ports the top-level session lifecycle from
// voice-agent/voice_agent/main.py (entrypoint): open the gRPC session with a
// VoiceAgentSessionStart, consume the SessionAck (canonical voice, avatar
// persona id, initial audio/video modes), join the LiveKit room, run until
// the room closes, and announce VoiceAgentSessionEnd on teardown.
//
// The audio loop (STT/TTS, turn-taking, barge-in) is OUT of scope for #454
// and lands in #455; the persona/grounding parity beyond reading the ack is
// #456; the realtime executor is #457; MCP/output is #458; lifecycle
// guardrails are #459; the avatar is #460. Those seams are marked below.

// sessionStartTimeout bounds how long we wait for the SessionAck after
// sending VoiceAgentSessionStart. Generous enough for a slow cognition node
// but bounded so a wedged backend fails the session bring-up loudly.
const sessionStartTimeout = 30 * time.Second

// sessionEndTimeout bounds the best-effort VoiceAgentSessionEnd notify on
// teardown so a slow backend can't wedge shutdown.
const sessionEndTimeout = 5 * time.Second

// SessionAck is the resolved subset of VoiceAgentSessionAck the session
// lifecycle acts on: the canonical voice, avatar persona id, and initial
// audio/video gate modes the backend decided from the GA's controls +
// override rows. The audio loop (#455) and avatar (#460) consume these.
type SessionAck struct {
	CanonicalVoice  string
	AvatarPersonaID string
	InitialAudio    string // "always_on" | "always_off" | "mirror_user"
	InitialVideo    string
}

// RoomJoiner joins the LiveKit room for a session and blocks until the room
// closes or ctx is cancelled, returning the close reason. It is implemented
// by the voice-tagged room-join code (room_voice.go, CGO/libopus) in the
// voice build, and by a no-op stub in the default build so the session
// lifecycle and its tests stay CGO-free. The session lifecycle owns the
// gRPC + ack handshake; the joiner owns only the media-participant lifetime.
type RoomJoiner interface {
	// JoinAndServe connects to the room named by req.RoomName using a minted
	// LiveKit token, confirms the connection, and blocks until the room
	// disconnects or ctx is done. It returns a short reason string suitable
	// for the VoiceAgentSessionEnd ("normal" / "error" / "inactivity").
	JoinAndServe(ctx context.Context, req RoomRequest) (reason string, err error)
}

// RoomRequest carries everything the room joiner needs: the room/space
// identity and the resolved ack. The LiveKit credentials come from Config.
type RoomRequest struct {
	SpaceID   string
	GaAgentID string
	RoomName  string
	Ack       SessionAck

	// RegisterSpeakSink lets the room joiner register the cascade (#455)
	// as the VoiceAgentSpeak consumer once the audio loop is constructed,
	// so unsolicited speak pushes reach TTS playout. nil-safe: the
	// CGO-free build never calls it. Set by Session.Run.
	RegisterSpeakSink func(SpeakSink)
}

// Session drives one voice-agent session against a single memQL space.
type Session struct {
	cfg    Config
	client *Client
	joiner RoomJoiner
	logger *slog.Logger

	spaceID   string
	gaAgentID string
	roomName  string

	// speakSink, when set, receives unsolicited VoiceAgentSpeak pushes so
	// the cascade audio loop (#455) can drive TTS playout. The voice-build
	// room joiner registers the cascade here once it is constructed; in the
	// default build it stays nil and pushes are logged only. Guarded by
	// speakMu since the read-loop goroutine (onVoiceAgentSpeak) and the
	// joiner goroutine (SetSpeakSink) touch it from different goroutines.
	speakMu   sync.Mutex
	speakSink SpeakSink
}

// SpeakSink consumes an unsolicited VoiceAgentSpeak directive. It is
// implemented by the cascade orchestrator (#455). Defined on the session
// so the voice-build room joiner can register the cascade without the
// CGO-free session lifecycle importing the media layer.
type SpeakSink interface {
	Speak(SpeakDirective) bool
}

// SetSpeakSink registers the VoiceAgentSpeak consumer (the cascade). Safe
// for concurrent use with onVoiceAgentSpeak.
func (s *Session) SetSpeakSink(sink SpeakSink) {
	s.speakMu.Lock()
	s.speakSink = sink
	s.speakMu.Unlock()
}

// NewSession builds a session for the given space. roomName is the LiveKit
// room name (the memQL convention is "polyphon-<spaceId>"); spaceId is
// derived by stripping that prefix, matching the Python agent so the value
// sent to memQL matches the participant row's spaceID exactly. The joiner
// may be nil, in which case the session runs the gRPC handshake and returns
// immediately after the ack (used by the default CGO-free build + tests; the
// voice build supplies a real RoomJoiner).
func NewSession(cfg Config, client *Client, roomName string, joiner RoomJoiner, logger *slog.Logger) *Session {
	spaceID := strings.TrimPrefix(roomName, "polyphon-")
	// The voice-agent sends a placeholder ga_agent_id of "<spaceId>-ga"; the
	// server resolves the real canonical agent id from the space's group-GA
	// participant (see handleVoiceAgentSessionStart). The real wiring through
	// the LiveKit token metadata is a follow-up (#456).
	gaAgentID := spaceID + "-ga"
	return &Session{
		cfg:       cfg,
		client:    client,
		joiner:    joiner,
		logger:    logger,
		spaceID:   spaceID,
		gaAgentID: gaAgentID,
		roomName:  roomName,
	}
}

// Run executes the full session lifecycle: connect the gRPC client, open the
// session (Start -> Ack), register the VoiceAgentSpeak push seam, join the
// room (if a joiner is wired), then announce SessionEnd on teardown. It
// blocks until the room closes, ctx is cancelled, or an unrecoverable error
// occurs. SessionEnd is always attempted (best-effort) before returning.
func (s *Session) Run(ctx context.Context) error {
	if err := s.client.Connect(ctx); err != nil {
		return err
	}
	defer s.client.Close()

	ack, err := s.start(ctx)
	if err != nil {
		// Best-effort end notify even on a failed start so audit sees the
		// attempt; reason "error".
		s.end(context.Background(), "error")
		return err
	}

	// Register the unsolicited VoiceAgentSpeak handler. memQL pushes one
	// Speak per SI reply utterance that lands while audio output is enabled
	// and no VoiceAgentTurnRequest is in flight; the audio loop (#455) drives
	// TTS playout from it. For #454 we register the seam and log receipt so
	// the wire path is provably reachable; the actual say() pipeline is #455.
	s.client.SetPushHandler("VoiceAgentSpeak", s.onVoiceAgentSpeak)

	reason := "normal"
	if s.joiner != nil {
		req := RoomRequest{
			SpaceID:           s.spaceID,
			GaAgentID:         s.gaAgentID,
			RoomName:          s.roomName,
			Ack:               ack,
			RegisterSpeakSink: s.SetSpeakSink,
		}
		r, joinErr := s.joiner.JoinAndServe(ctx, req)
		if r != "" {
			reason = r
		}
		if joinErr != nil {
			reason = "error"
			s.end(context.Background(), reason)
			return joinErr
		}
	} else if s.logger != nil {
		// Default (CGO-free) build path: no room joiner wired. The session
		// handshake is complete; the media loop is the voice build's job.
		s.logger.Info("voice-agent session ack received; no room joiner wired (audio loop is #455)",
			"space_id", s.spaceID, "canonical_voice", ack.CanonicalVoice)
	}

	s.end(context.Background(), reason)
	return nil
}

// start sends VoiceAgentSessionStart and consumes the SessionAck. It returns
// the resolved ack or an error when the backend reports failure / the ack
// times out.
func (s *Session) start(ctx context.Context) (SessionAck, error) {
	startCtx, cancel := context.WithTimeout(ctx, sessionStartTimeout)
	defer cancel()

	if s.logger != nil {
		s.logger.Info("voice-agent session start",
			"space_id", s.spaceID, "ga_agent_id", s.gaAgentID,
			"room_name", s.roomName, "avatar_vendor", s.cfg.AvatarVendor)
	}

	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentSessionStart{
			VoiceAgentSessionStart: &memqlv1.VoiceAgentSessionStart{
				SpaceId:      s.spaceID,
				GaAgentId:    s.gaAgentID,
				RoomName:     s.roomName,
				AvatarVendor: s.cfg.AvatarVendor,
			},
		},
	}
	reply, err := s.client.SendRequest(startCtx, envelope)
	if err != nil {
		return SessionAck{}, fmt.Errorf("voice-agent session start: %w", err)
	}

	ackMsg := reply.GetVoiceAgentSessionAck()
	if ackMsg == nil {
		return SessionAck{}, fmt.Errorf(
			"voice-agent session start: expected VoiceAgentSessionAck, got %s",
			ServerPayloadName(reply))
	}
	if !ackMsg.GetSuccess() {
		return SessionAck{}, fmt.Errorf(
			"voice-agent session start rejected (code=%s): %s",
			ackMsg.GetErrorCode(), ackMsg.GetErrorMessage())
	}

	ack := SessionAck{
		CanonicalVoice:  ackMsg.GetGaCanonicalVoice(),
		AvatarPersonaID: ackMsg.GetGaAvatarPersonaId(),
		InitialAudio:    ackMsg.GetInitialAudioMode(),
		InitialVideo:    ackMsg.GetInitialVideoMode(),
	}
	if s.logger != nil {
		s.logger.Info("voice-agent session ack",
			"canonical_voice", ack.CanonicalVoice,
			"avatar_persona", ack.AvatarPersonaID,
			"initial_audio", ack.InitialAudio,
			"initial_video", ack.InitialVideo)
	}
	return ack, nil
}

// end announces VoiceAgentSessionEnd to memQL for audit + cleanup.
// Best-effort: failures are logged, never returned, so they can't wedge
// teardown.
func (s *Session) end(ctx context.Context, reason string) {
	endCtx, cancel := context.WithTimeout(ctx, sessionEndTimeout)
	defer cancel()
	envelope := &memqlv1.MemqlClientMessage{
		Payload: &memqlv1.MemqlClientMessage_VoiceAgentSessionEnd{
			VoiceAgentSessionEnd: &memqlv1.VoiceAgentSessionEnd{
				SpaceId:  s.spaceID,
				RoomName: s.roomName,
				Reason:   reason,
			},
		},
	}
	if err := s.client.Send(endCtx, envelope); err != nil && s.logger != nil {
		s.logger.Warn("voice-agent session end notify failed", "reason", reason, "err", err)
	}
}

// onVoiceAgentSpeak is the VoiceAgentSpeak push seam. For #454 it logs
// receipt so the unsolicited-push wire path is provably reachable end to end;
// the TTS say() / barge-in pipeline that turns this into audible output is
// #455.
func (s *Session) onVoiceAgentSpeak(envelope *memqlv1.MemqlServerMessage) error {
	speak := envelope.GetVoiceAgentSpeak()
	if speak == nil {
		return nil
	}
	text := strings.TrimSpace(speak.GetText())
	if text == "" {
		return nil
	}
	if s.logger != nil {
		s.logger.Info("voice-agent speak request",
			"request_id", speak.GetRequestId(),
			"utterance_id", speak.GetUtteranceId(),
			"text_len", len(text))
	}
	// Forward to the cascade audio loop (#455) when wired (voice build).
	// The cascade drives the conductor-gated TTS playout (transition B,
	// the Go analog of main.py::_on_voice_agent_speak -> session.say). In
	// the default CGO-free build no sink is registered and the push is
	// logged only -- the wire path stays provably reachable.
	s.speakMu.Lock()
	sink := s.speakSink
	s.speakMu.Unlock()
	if sink != nil {
		sink.Speak(SpeakDirective{
			Text:        text,
			UtteranceID: speak.GetUtteranceId(),
			RequestID:   speak.GetRequestId(),
		})
	}
	return nil
}
