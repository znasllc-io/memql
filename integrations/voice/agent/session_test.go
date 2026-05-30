package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// recordingJoiner is a test RoomJoiner that records the RoomRequest it
// received and returns a configurable reason/err, blocking until released so
// the test can assert the SessionEnd ordering.
type recordingJoiner struct {
	mu      sync.Mutex
	req     RoomRequest
	called  bool
	reason  string
	err     error
	release chan struct{}
}

func (j *recordingJoiner) JoinAndServe(ctx context.Context, req RoomRequest) (string, error) {
	j.mu.Lock()
	j.req = req
	j.called = true
	rel := j.release
	j.mu.Unlock()
	if rel != nil {
		select {
		case <-rel:
		case <-ctx.Done():
			return "normal", nil
		}
	}
	return j.reason, j.err
}

// ackEcho wires a fakeStream so a VoiceAgentSessionStart is answered with a
// SessionAck carrying the given fields, and any other request is acked
// trivially.
func ackEcho(fs *fakeStream, ack *memqlv1.VoiceAgentSessionAck) {
	fs.onSend = func(env *memqlv1.MemqlClientMessage) {
		if env.GetVoiceAgentSessionStart() != nil {
			fs.push(&memqlv1.MemqlServerMessage{
				CorrelateTo: env.GetMessageId(),
				Payload:     &memqlv1.MemqlServerMessage_VoiceAgentSessionAck{VoiceAgentSessionAck: ack},
			})
		}
	}
}

func TestSession_Run_HandshakeAndEnd(t *testing.T) {
	fs := newFakeStream()
	ackEcho(fs, &memqlv1.VoiceAgentSessionAck{
		Success:           true,
		GaCanonicalVoice:  "alto",
		GaAvatarPersonaId: "persona-1",
		InitialAudioMode:  "always_on",
		InitialVideoMode:  "always_off",
	})
	client := NewClient("test:0", "tok", fakeDialer(fs), nil)

	joiner := &recordingJoiner{reason: "normal", release: make(chan struct{})}
	close(joiner.release) // return immediately

	sess := NewSession(Config{AvatarVendor: "anam"}, client, "polyphon-space-42", joiner, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, sess.Run(ctx))

	// spaceId is the room name minus the polyphon- prefix; gaAgentId is
	// "<spaceId>-ga".
	joiner.mu.Lock()
	req := joiner.req
	called := joiner.called
	joiner.mu.Unlock()
	require.True(t, called)
	assert.Equal(t, "space-42", req.SpaceID)
	assert.Equal(t, "space-42-ga", req.GaAgentID)
	assert.Equal(t, "polyphon-space-42", req.RoomName)
	assert.Equal(t, "alto", req.Ack.CanonicalVoice)
	assert.Equal(t, "persona-1", req.Ack.AvatarPersonaID)
	assert.Equal(t, "always_on", req.Ack.InitialAudio)

	// The wire carried exactly a SessionStart then a SessionEnd, in order.
	sent := fs.sentEnvelopes()
	require.GreaterOrEqual(t, len(sent), 2)
	assert.NotNil(t, sent[0].GetVoiceAgentSessionStart())
	last := sent[len(sent)-1]
	require.NotNil(t, last.GetVoiceAgentSessionEnd())
	assert.Equal(t, "space-42", last.GetVoiceAgentSessionEnd().GetSpaceId())
	assert.Equal(t, "normal", last.GetVoiceAgentSessionEnd().GetReason())
}

func TestSession_Run_NoJoiner_HandshakeOnly(t *testing.T) {
	fs := newFakeStream()
	ackEcho(fs, &memqlv1.VoiceAgentSessionAck{Success: true, GaCanonicalVoice: "alto"})
	client := NewClient("test:0", "tok", fakeDialer(fs), nil)

	sess := NewSession(Config{AvatarVendor: "none"}, client, "polyphon-s1", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, sess.Run(ctx))

	sent := fs.sentEnvelopes()
	require.GreaterOrEqual(t, len(sent), 2)
	assert.NotNil(t, sent[0].GetVoiceAgentSessionStart())
	assert.NotNil(t, sent[len(sent)-1].GetVoiceAgentSessionEnd())
}

func TestSession_Run_AckFailureSurfaces(t *testing.T) {
	fs := newFakeStream()
	ackEcho(fs, &memqlv1.VoiceAgentSessionAck{
		Success:      false,
		ErrorCode:    "bad_space",
		ErrorMessage: "space not found",
	})
	client := NewClient("test:0", "tok", fakeDialer(fs), nil)

	joiner := &recordingJoiner{}
	sess := NewSession(Config{}, client, "polyphon-s1", joiner, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := sess.Run(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad_space")

	// The joiner must NOT have been called when the ack failed.
	joiner.mu.Lock()
	called := joiner.called
	joiner.mu.Unlock()
	assert.False(t, called)

	// A SessionEnd with reason "error" should still have been attempted.
	sent := fs.sentEnvelopes()
	var sawErrorEnd bool
	for _, e := range sent {
		if end := e.GetVoiceAgentSessionEnd(); end != nil && end.GetReason() == "error" {
			sawErrorEnd = true
		}
	}
	assert.True(t, sawErrorEnd, "expected a SessionEnd(reason=error) after ack failure")
}

func TestSession_Run_JoinErrorReportsError(t *testing.T) {
	fs := newFakeStream()
	ackEcho(fs, &memqlv1.VoiceAgentSessionAck{Success: true})
	client := NewClient("test:0", "tok", fakeDialer(fs), nil)

	joiner := &recordingJoiner{err: assertErr{}, release: make(chan struct{})}
	close(joiner.release)
	sess := NewSession(Config{}, client, "polyphon-s1", joiner, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.Error(t, sess.Run(ctx))

	sent := fs.sentEnvelopes()
	last := sent[len(sent)-1]
	require.NotNil(t, last.GetVoiceAgentSessionEnd())
	assert.Equal(t, "error", last.GetVoiceAgentSessionEnd().GetReason())
}

func TestSession_VoiceAgentSpeakSeam(t *testing.T) {
	// The speak handler is registered after the ack; verify it tolerates a
	// push without panicking (the audio playout itself is #455). We invoke
	// the handler directly to assert the parse/guard logic.
	sess := NewSession(Config{}, NewClient("x", "y", fakeDialer(newFakeStream()), nil), "polyphon-s1", nil, nil)
	require.NoError(t, sess.onVoiceAgentSpeak(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSpeak{
			VoiceAgentSpeak: &memqlv1.VoiceAgentSpeak{RequestId: "r", Text: "hi"},
		},
	}))
	// Empty text is a no-op, not an error.
	require.NoError(t, sess.onVoiceAgentSpeak(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_VoiceAgentSpeak{VoiceAgentSpeak: &memqlv1.VoiceAgentSpeak{}},
	}))
}

func TestSession_NewSession_StripsRoomPrefix(t *testing.T) {
	sess := NewSession(Config{}, NewClient("x", "y", fakeDialer(newFakeStream()), nil), "polyphon-daily-standup", nil, nil)
	assert.Equal(t, "daily-standup", sess.spaceID)
	assert.Equal(t, "daily-standup-ga", sess.gaAgentID)

	// A room name without the prefix is used verbatim.
	sess2 := NewSession(Config{}, NewClient("x", "y", fakeDialer(newFakeStream()), nil), "raw-room", nil, nil)
	assert.Equal(t, "raw-room", sess2.spaceID)
}

// assertErr is a tiny error type for the join-error test.
type assertErr struct{}

func (assertErr) Error() string { return "join failed" }
