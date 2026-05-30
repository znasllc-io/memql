package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// avatar_dispatch_test.go covers the pure vendor-selection + gating rules
// ported from avatar_plugin.py: persona-stamped vendor wins, runtime default
// fallback, video gating -> audio-only, and the per-vendor persona-id
// resolution.

func videoOnPersona(vendor, personaID string) Persona {
	return Persona{
		AvatarVendor:     vendor,
		AvatarPersonaID:  personaID,
		InitialAudioMode: "always_on",
		InitialVideoMode: "always_on",
	}
}

func TestResolveAvatarPlan_PersonaStampedVendorWins(t *testing.T) {
	// Runtime default is simli, but the persona is stamped anam -> anam wins,
	// so a persona minted against Anam is never silently re-rendered on Simli.
	ac := AvatarConfig{Vendor: "simli", AnamAPIKey: "ak", SimliAPIKey: "sk"}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("anam", "persona-1"))
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, avatarVendorAnam, plan.Vendor)
	assert.Equal(t, "persona-1", plan.PersonaID)
}

func TestResolveAvatarPlan_RuntimeDefaultFallback(t *testing.T) {
	// Persona carries no vendor -> fall through to the runtime default (anam).
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "ak"}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("", "persona-2"))
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, avatarVendorAnam, plan.Vendor)
	assert.Equal(t, "persona-2", plan.PersonaID)
}

func TestResolveAvatarPlan_VideoGatedOffIsAudioOnly(t *testing.T) {
	// Video disabled -> no avatar at all (cost-saving gate), even though a
	// vendor + persona id are present. (nil, nil) == audio-only.
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "ak"}
	persona := Persona{
		AvatarVendor:     "anam",
		AvatarPersonaID:  "persona-3",
		InitialAudioMode: "always_on",
		InitialVideoMode: "always_off",
	}
	plan, err := ResolveAvatarPlan(ac, persona)
	require.NoError(t, err)
	assert.Nil(t, plan)
}

func TestResolveAvatarPlan_VendorNoneIsAudioOnly(t *testing.T) {
	ac := AvatarConfig{Vendor: "none", AnamAPIKey: "ak"}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("", "persona-4"))
	require.NoError(t, err)
	assert.Nil(t, plan)
}

func TestResolveAvatarPlan_AnamDefaultPersonaIDFallback(t *testing.T) {
	// No per-agent persona id; platform default personaId path is preferred
	// over the bare avatarId.
	ac := AvatarConfig{
		Vendor:                "anam",
		AnamAPIKey:            "ak",
		AnamDefaultPersonaID:  "default-persona",
		AnamDefaultAvatarID:   "default-avatar",
		AnamDefaultPersonaNam: "Liv",
	}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("", ""))
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, "default-persona", plan.PersonaID)
	assert.Empty(t, plan.AvatarID)
	assert.Equal(t, "Liv", plan.DisplayName)
}

func TestResolveAvatarPlan_AnamBareAvatarIDFallback(t *testing.T) {
	// No persona id anywhere; the bare default avatarId path is the last resort.
	ac := AvatarConfig{
		Vendor:              "anam",
		AnamAPIKey:          "ak",
		AnamDefaultAvatarID: "default-avatar",
	}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("", ""))
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Empty(t, plan.PersonaID)
	assert.Equal(t, "default-avatar", plan.AvatarID)
}

func TestResolveAvatarPlan_AnamNoIdIsAudioOnly(t *testing.T) {
	// Anam selected but no persona id on the agent and no platform default ->
	// audio-only (not an error).
	ac := AvatarConfig{Vendor: "anam", AnamAPIKey: "ak"}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("", ""))
	require.NoError(t, err)
	assert.Nil(t, plan)
}

func TestResolveAvatarPlan_AnamMissingKeyIsError(t *testing.T) {
	// Selected vendor with no API key is a hard config mismatch -- surfaced so
	// we don't silently publish nothing.
	ac := AvatarConfig{Vendor: "anam"}
	_, err := ResolveAvatarPlan(ac, videoOnPersona("anam", "persona-5"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ANAM_API_KEY")
}

func TestResolveAvatarPlan_SimliFaceIdAndKey(t *testing.T) {
	ac := AvatarConfig{Vendor: "simli", SimliAPIKey: "sk"}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("simli", "face-9"))
	require.NoError(t, err)
	require.NotNil(t, plan)
	assert.Equal(t, avatarVendorSimli, plan.Vendor)
	assert.Equal(t, "face-9", plan.PersonaID)
}

func TestResolveAvatarPlan_SimliNoFaceIdIsAudioOnly(t *testing.T) {
	// Simli needs an explicit face id; without one it is audio-only, not an
	// error (matches the Python info-log path).
	ac := AvatarConfig{Vendor: "simli", SimliAPIKey: "sk"}
	plan, err := ResolveAvatarPlan(ac, videoOnPersona("simli", ""))
	require.NoError(t, err)
	assert.Nil(t, plan)
}

func TestResolveAvatarPlan_SimliMissingKeyIsError(t *testing.T) {
	ac := AvatarConfig{Vendor: "simli"}
	_, err := ResolveAvatarPlan(ac, videoOnPersona("simli", "face-9"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SIMLI_API_KEY")
}

func TestVendorForPersona(t *testing.T) {
	assert.Equal(t, "anam", vendorForPersona("anam", "simli"))
	assert.Equal(t, "simli", vendorForPersona("simli", "anam"))
	assert.Equal(t, "anam", vendorForPersona("", "anam"))
	assert.Equal(t, "simli", vendorForPersona("garbage", "simli"))
	assert.Equal(t, "anam", vendorForPersona(" ANAM ", "simli"))
}
