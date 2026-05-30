package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// avatar_start_test.go covers the CGO-free half of the audio-source-agnostic
// start seam (startAvatarSession): the no-op / non-fatal / success contract the
// voice-tagged room glue relies on. The vendor client is faked so no network or
// LiveKit media is needed -- the same start core serves cascade and realtime
// audio because it only ever deals in PCM sinks downstream, never the executor.

type fakeVendorClient struct {
	res         avatarStartResult
	err         error
	startedWith avatarStartParams
}

func (f *fakeVendorClient) Start(_ context.Context, roomName, livekitURL, livekitToken string) (avatarStartResult, error) {
	f.startedWith = avatarStartParams{RoomName: roomName, LiveKitURL: livekitURL, LiveKitToken: livekitToken}
	return f.res, f.err
}

func TestStartAvatarSession_NoOpWhenDisabled(t *testing.T) {
	// vendor=none -> plan nil -> not started, no error, vendor client never
	// constructed.
	ac := AvatarConfig{Vendor: "none"}
	called := false
	res, started, err := startAvatarSession(context.Background(), ac, videoOnPersona("", "p"),
		avatarStartParams{}, func(AvatarPlan) (avatarVendorClient, error) {
			called = true
			return &fakeVendorClient{}, nil
		})
	require.NoError(t, err)
	assert.False(t, started)
	assert.Equal(t, avatarStartResult{}, res)
	assert.False(t, called, "vendor client must not be built when avatar disabled")
}

func TestStartAvatarSession_NoOpWhenVideoGated(t *testing.T) {
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "k"}
	persona := Persona{AvatarVendor: "anam", AvatarPersonaID: "p", InitialAudioMode: "always_on", InitialVideoMode: "always_off"}
	_, started, err := startAvatarSession(context.Background(), ac, persona, avatarStartParams{}, nil)
	require.NoError(t, err)
	assert.False(t, started)
}

func TestStartAvatarSession_ConfigMismatchIsError(t *testing.T) {
	// Selected vendor, missing key -> error surfaced (caller logs + audio-only).
	ac := AvatarConfig{Vendor: "anam"}
	_, started, err := startAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"), avatarStartParams{}, nil)
	require.Error(t, err)
	assert.False(t, started)
}

func TestStartAvatarSession_VendorFailureIsNonFatal(t *testing.T) {
	// The vendor Start fails -> startAvatarSession returns (false, err); the
	// caller treats it as non-fatal (audio-only). started is false.
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "k"}
	fake := &fakeVendorClient{err: errors.New("anam 503")}
	_, started, err := startAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"),
		avatarStartParams{RoomName: "r", LiveKitURL: "u", LiveKitToken: "t"},
		func(AvatarPlan) (avatarVendorClient, error) { return fake, nil })
	require.Error(t, err)
	assert.False(t, started)
}

func TestStartAvatarSession_SuccessReturnsIdentity(t *testing.T) {
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "k"}
	fake := &fakeVendorClient{res: avatarStartResult{SessionID: "s1", AvatarIdentity: "avatar-agent", LiveKitSampleRate: 16000}}
	res, started, err := startAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"),
		avatarStartParams{RoomName: "polyphon-x", LiveKitURL: "wss://pub", LiveKitToken: "jwt"},
		func(AvatarPlan) (avatarVendorClient, error) { return fake, nil })
	require.NoError(t, err)
	assert.True(t, started)
	assert.Equal(t, "s1", res.SessionID)
	assert.Equal(t, "avatar-agent", res.AvatarIdentity)
	assert.Equal(t, 16000, res.LiveKitSampleRate)
	// The minted LiveKit token + public URL were handed to the vendor verbatim.
	assert.Equal(t, "wss://pub", fake.startedWith.LiveKitURL)
	assert.Equal(t, "jwt", fake.startedWith.LiveKitToken)
}

func TestStartAvatarSession_DefaultsSampleRateAndIdentity(t *testing.T) {
	// A vendor that returns no identity / sample rate gets the package defaults.
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "k"}
	fake := &fakeVendorClient{res: avatarStartResult{SessionID: "s"}}
	res, started, err := startAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"),
		avatarStartParams{}, func(AvatarPlan) (avatarVendorClient, error) { return fake, nil })
	require.NoError(t, err)
	require.True(t, started)
	assert.Equal(t, avatarParticipantIdentity, res.AvatarIdentity)
	assert.Equal(t, avatarPCMSampleRate, res.LiveKitSampleRate)
}
