package avatarvendor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// start_test.go covers the CGO-free half of the audio-source-agnostic start
// seam (StartAvatarSession): the no-op / non-fatal / success contract the
// callers (voice room glue / direct-path capability) rely on. The vendor client
// is faked so no network or LiveKit media is needed.

type fakeVendorClient struct {
	res         AvatarStartResult
	err         error
	startedWith AvatarStartParams
}

func (f *fakeVendorClient) Start(_ context.Context, roomName, livekitURL, livekitToken string) (AvatarStartResult, error) {
	f.startedWith = AvatarStartParams{RoomName: roomName, LiveKitURL: livekitURL, LiveKitToken: livekitToken}
	return f.res, f.err
}

func TestStartAvatarSession_NoOpWhenDisabled(t *testing.T) {
	// vendor=none -> plan nil -> not started, no error, vendor client never
	// constructed.
	ac := AvatarConfig{Vendor: "none"}
	called := false
	res, started, err := StartAvatarSession(context.Background(), ac, videoOnPersona("", "p"),
		AvatarStartParams{}, func(AvatarPlan) (AvatarVendorClient, error) {
			called = true
			return &fakeVendorClient{}, nil
		})
	require.NoError(t, err)
	assert.False(t, started)
	assert.Equal(t, AvatarStartResult{}, res)
	assert.False(t, called, "vendor client must not be built when avatar disabled")
}

func TestStartAvatarSession_NoOpWhenVideoGated(t *testing.T) {
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "k"}
	persona := PersonaInput{AvatarVendor: "anam", AvatarPersonaID: "p", VideoEnabled: false}
	_, started, err := StartAvatarSession(context.Background(), ac, persona, AvatarStartParams{}, nil)
	require.NoError(t, err)
	assert.False(t, started)
}

func TestStartAvatarSession_ConfigMismatchIsError(t *testing.T) {
	// Selected vendor, missing key -> error surfaced (caller logs + audio-only).
	ac := AvatarConfig{Vendor: "anam"}
	_, started, err := StartAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"), AvatarStartParams{}, nil)
	require.Error(t, err)
	assert.False(t, started)
}

func TestStartAvatarSession_VendorFailureIsNonFatal(t *testing.T) {
	// The vendor Start fails -> StartAvatarSession returns (false, err); the
	// caller treats it as non-fatal (audio-only). started is false.
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "k"}
	fake := &fakeVendorClient{err: errors.New("anam 503")}
	_, started, err := StartAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"),
		AvatarStartParams{RoomName: "r", LiveKitURL: "u", LiveKitToken: "t"},
		func(AvatarPlan) (AvatarVendorClient, error) { return fake, nil })
	require.Error(t, err)
	assert.False(t, started)
}

func TestStartAvatarSession_SuccessReturnsIdentity(t *testing.T) {
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "k"}
	fake := &fakeVendorClient{res: AvatarStartResult{SessionID: "s1", AvatarIdentity: "avatar-agent", LiveKitSampleRate: 16000}}
	res, started, err := StartAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"),
		AvatarStartParams{RoomName: "polyphon-x", LiveKitURL: "wss://pub", LiveKitToken: "jwt"},
		func(AvatarPlan) (AvatarVendorClient, error) { return fake, nil })
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
	fake := &fakeVendorClient{res: AvatarStartResult{SessionID: "s"}}
	res, started, err := StartAvatarSession(context.Background(), ac, videoOnPersona("anam", "p"),
		AvatarStartParams{}, func(AvatarPlan) (AvatarVendorClient, error) { return fake, nil })
	require.NoError(t, err)
	require.True(t, started)
	assert.Equal(t, AvatarParticipantIdentity, res.AvatarIdentity)
	assert.Equal(t, DefaultPCMSampleRate, res.LiveKitSampleRate)
}
