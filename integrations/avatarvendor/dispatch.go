package avatarvendor

// dispatch.go is the pure vendor-selection + gating logic that decides, from
// the AvatarConfig and the resolved PersonaInput, WHICH vendor (if any) renders
// this session's avatar and with which persona/avatar id. It is CGO-free and
// fully unit-tested in the default lane; no network, no media. Ported 1:1 from
// the voice-agent's avatar_dispatch.go (itself the port of the Python
// avatar_plugin.py).
//
// The rules:
//   - vendor-for-persona: a vendor stamped at persona-mint time wins, so a
//     persona minted against Anam still rides Anam even when the runtime
//     default is Simli. Falls through to the runtime default vendor when the
//     persona record carries no vendor.
//   - video gating: when the resolved persona has video disabled (the avatar
//     would be a talking head no one sees), no avatar is built -- audio-only,
//     saving the vendor cost.
//   - vendor=none / no usable persona id: no avatar (audio-only).
//   - vendor-config mismatch (e.g. anam selected but ANAM_API_KEY unset)
//     returns an error so we don't silently publish nothing.

import (
	"fmt"
	"strings"
)

// vendorForPersona picks the vendor that should drive THIS persona. A vendor
// stamped on the persona record wins; otherwise the runtime default vendor is
// used.
func vendorForPersona(personaVendor, runtimeVendor string) string {
	pv := strings.ToLower(strings.TrimSpace(personaVendor))
	if pv == string(avatarVendorAnam) || pv == string(avatarVendorSimli) {
		return pv
	}
	return strings.ToLower(strings.TrimSpace(runtimeVendor))
}

// ResolveAvatarPlan decides whether and how to drive an avatar for one session.
// It returns (nil, nil) for the audio-only paths (vendor=none, video gated off,
// no usable persona id) and a non-nil plan when an avatar should be built. A
// non-nil error is a hard config mismatch (selected vendor with no API key)
// the caller should surface.
//
// This resolves the PLAN; the vendor client (anamClient / simliClient) performs
// the REST mint from it. Splitting plan-resolution from REST keeps the dispatch
// rules pure and testable.
func ResolveAvatarPlan(ac AvatarConfig, persona PersonaInput) (*AvatarPlan, error) {
	// Video gating: a session with video disabled gets no avatar at all. This
	// is the cost-saving gate; it must run before any vendor work. The caller's
	// persona resolver already forced video off when audio is hard-off, so this
	// single check covers both the explicit always_off video and the
	// audio-forced-off case.
	if !persona.VideoEnabled {
		return nil, nil
	}

	runtimeVendor := strings.ToLower(strings.TrimSpace(ac.Vendor))
	if runtimeVendor == "none" || runtimeVendor == "" {
		return nil, nil
	}

	vendor := vendorForPersona(persona.AvatarVendor, runtimeVendor)
	switch vendor {
	case string(avatarVendorAnam):
		return resolveAnamPlan(ac, persona)
	case string(avatarVendorSimli):
		return resolveSimliPlan(ac, persona)
	default:
		return nil, fmt.Errorf("avatar: unknown vendor %q", vendor)
	}
}

// resolveAnamPlan resolves the Anam plan: per-agent stamped persona id wins;
// then the platform default personaId; then the bare default avatarId; else no
// avatar. ANAM_API_KEY is required whenever Anam is selected.
func resolveAnamPlan(ac AvatarConfig, persona PersonaInput) (*AvatarPlan, error) {
	if strings.TrimSpace(ac.AnamAPIKey) == "" {
		return nil, fmt.Errorf("avatar: ANAM_API_KEY required when avatarVendor=anam")
	}
	name := strings.TrimSpace(ac.AnamDefaultPersonaNam)
	if name == "" {
		name = AvatarParticipantName
	}
	base := AvatarPlan{
		Vendor:      avatarVendorAnam,
		APIKey:      ac.AnamAPIKey,
		APIBase:     anamAPIBase,
		DisplayName: name,
	}

	// 1. Per-agent stamped persona id (treated as an Anam personaId).
	if id := strings.TrimSpace(persona.AvatarPersonaID); id != "" {
		base.PersonaID = id
		return &base, nil
	}
	// 2. Platform default: personaId path preferred.
	if id := strings.TrimSpace(ac.AnamDefaultPersonaID); id != "" {
		base.PersonaID = id
		return &base, nil
	}
	// 3. Platform default: bare avatarId via ephemeral PersonaConfig.
	if id := strings.TrimSpace(ac.AnamDefaultAvatarID); id != "" {
		base.AvatarID = id
		return &base, nil
	}
	// 4. No persona id on the agent and no platform default -> audio-only.
	return nil, nil
}

// resolveSimliPlan resolves the Simli plan: Simli needs an explicit face id
// (the persona's AvatarPersonaID), and SIMLI_API_KEY when selected. No face id
// -> audio-only (not an error).
func resolveSimliPlan(ac AvatarConfig, persona PersonaInput) (*AvatarPlan, error) {
	if strings.TrimSpace(ac.SimliAPIKey) == "" {
		return nil, fmt.Errorf("avatar: SIMLI_API_KEY required when avatarVendor=simli")
	}
	faceID := strings.TrimSpace(persona.AvatarPersonaID)
	if faceID == "" {
		return nil, nil
	}
	return &AvatarPlan{
		Vendor:      avatarVendorSimli,
		PersonaID:   faceID,
		APIKey:      ac.SimliAPIKey,
		APIBase:     simliAPIBase,
		DisplayName: AvatarParticipantName,
	}, nil
}
