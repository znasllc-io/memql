package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolvePersona_FromAck(t *testing.T) {
	ack := SessionAck{
		CanonicalVoice:  "tenor",
		AvatarPersonaID: "persona-7",
		InitialAudio:    "always_on",
		InitialVideo:    "always_on",
	}
	p := ResolvePersona(ack, Config{AvatarVendor: "anam"})

	assert.Equal(t, "tenor", p.CanonicalVoice)
	assert.Equal(t, "persona-7", p.AvatarPersonaID)
	assert.Equal(t, "anam", p.AvatarVendor)
	assert.Equal(t, "always_on", p.InitialAudioMode)
	assert.Equal(t, "always_on", p.InitialVideoMode)
	assert.True(t, p.AvatarEnabled())
	assert.True(t, p.AudioEnabled())
	assert.True(t, p.VideoEnabled())
}

func TestResolvePersona_NeutralDefaultsWhenUnset(t *testing.T) {
	// An empty ack (no voice, no avatar, no modes) resolves to the neutral
	// defaults: alto canonical voice, mirror_user audio/video, no avatar.
	p := ResolvePersona(SessionAck{}, Config{AvatarVendor: "none"})

	assert.Equal(t, "alto", p.CanonicalVoice)
	assert.Equal(t, "", p.AvatarPersonaID)
	assert.Equal(t, "none", p.AvatarVendor)
	assert.Equal(t, "mirror_user", p.InitialAudioMode)
	assert.Equal(t, "mirror_user", p.InitialVideoMode)
	assert.False(t, p.AvatarEnabled())
	// mirror_user is treated as enabled.
	assert.True(t, p.AudioEnabled())
	assert.True(t, p.VideoEnabled())

	// Persona-prompt fields default empty (not on the ack today).
	assert.Equal(t, "", p.DisplayName)
	assert.Equal(t, "", p.Role)
	assert.Equal(t, "", p.Description)
	assert.Equal(t, "", p.Style)
}

func TestResolvePersona_AudioOffGatesVideoOff(t *testing.T) {
	// Hard-off audio forces video hard-off even when the ack asked for
	// always_on video -- no talking head without a voice.
	ack := SessionAck{
		CanonicalVoice: "alto",
		InitialAudio:   "always_off",
		InitialVideo:   "always_on",
	}
	p := ResolvePersona(ack, Config{AvatarVendor: "anam"})

	assert.Equal(t, "always_off", p.InitialAudioMode)
	assert.Equal(t, "always_off", p.InitialVideoMode)
	assert.False(t, p.AudioEnabled())
	assert.False(t, p.VideoEnabled())
}

func TestResolvePersona_MirrorAudioDoesNotGateVideo(t *testing.T) {
	// mirror_user audio is NOT a hard-off, so it does not force video off.
	ack := SessionAck{
		CanonicalVoice: "alto",
		InitialAudio:   "mirror_user",
		InitialVideo:   "always_on",
	}
	p := ResolvePersona(ack, Config{AvatarVendor: "simli"})

	assert.Equal(t, "mirror_user", p.InitialAudioMode)
	assert.Equal(t, "always_on", p.InitialVideoMode)
}

func TestResolvePersona_AvatarEnabledRequiresVendorAndPersonaID(t *testing.T) {
	// Persona id present but vendor "none" -> not enabled.
	p := ResolvePersona(SessionAck{AvatarPersonaID: "p1"}, Config{AvatarVendor: "none"})
	assert.False(t, p.AvatarEnabled())

	// Vendor set but no persona id -> not enabled.
	p = ResolvePersona(SessionAck{}, Config{AvatarVendor: "anam"})
	assert.False(t, p.AvatarEnabled())

	// Both present -> enabled.
	p = ResolvePersona(SessionAck{AvatarPersonaID: "p1"}, Config{AvatarVendor: "anam"})
	assert.True(t, p.AvatarEnabled())
}
