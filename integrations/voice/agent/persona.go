package agent

// persona.go is the Go port of the Python voice-agent's persona_resolver.py:
// it turns the resolved SessionAck (canonical voice, avatar persona id,
// initial audio/video gate modes) into a single Persona value the rest of
// the agent consumes. The Python module owned the gRPC round-trip itself;
// in the Go agent the round-trip already happens in session.go's start()
// (one VoiceAgentSessionStart -> SessionAck), so this file is the pure
// resolution layer that runs ON the ack -- keeping it CGO-free and unit-
// testable without standing up a stream.
//
// The realtime executor (#457), the cascade audio loop (#455), and the
// avatar integration (#460) all consume the Persona this file produces;
// none of those are wired here. The persona-prompt fields (DisplayName /
// Role / Description / Style) are NOT carried on VoiceAgentSessionAck today
// (the proto stamps only voice / avatar / audio / video) so they resolve to
// the neutral defaults below until a future proto bump stamps them -- the
// same defensive degrade-to-neutral behaviour persona_resolver.py applies
// via getattr.

const (
	// defaultCanonicalVoice mirrors persona_resolver.py: an unset canonical
	// voice on the ack falls back to the GA's seeded canonical ("alto", see
	// voice.GAVoiceCanonical) rather than going silent.
	defaultCanonicalVoice = "alto"

	// defaultChannelMode mirrors the Python initial_audio_mode /
	// initial_video_mode fallback and the server-side resolveInitialChannelMode
	// fallback: when the ack carries no mode, mirror the user's own track.
	defaultChannelMode = "mirror_user"
)

// Persona is the resolved per-session runtime configuration for one GA, the
// Go analog of the Python voice-agent's persona_resolver.py::Persona.
//
// It is produced once at session bring-up by ResolvePersona(ack, cfg) and
// held for the lifetime of the LiveKit room. Mid-session state changes (the
// user flipping the GA mic/cam toggle) ride a separate override subscription
// on v1:cognition:{audio,video}override -- TODO(#455) wires that loop; this
// struct holds only the session-start snapshot.
type Persona struct {
	// CanonicalVoice is the stable, provider-independent voice name (alto /
	// tenor / ...). Always non-empty (defaults to defaultCanonicalVoice).
	CanonicalVoice string

	// AvatarPersonaID is the Anam / Simli persona id the avatar participant
	// (#460) renders. Empty when the GA has no avatar persona stamped.
	AvatarPersonaID string

	// AvatarVendor is the runtime avatar vendor ("anam" / "simli" / "none").
	// Sourced from Config (the ack does not carry it today -- see the Python
	// note about the Phase 11 follow-up), so plugin selection trusts the
	// process configuration.
	AvatarVendor string

	// InitialAudioMode / InitialVideoMode are the resolved publication gate
	// modes ("always_on" | "always_off" | "mirror_user") the backend decided
	// from the GA's audioControl/videoControl + active override rows. After
	// video gating (see ResolvePersona) InitialVideoMode is never "always_on"
	// while audio is "always_off".
	InitialAudioMode string
	InitialVideoMode string

	// Persona-prompt fields -- the identity + style the realtime executor
	// (#457) renders into its session instructions through the shared
	// agentdef.RenderIdentityBlock (#476/#480), the SAME block the cognition
	// text path renders, so voice and text cannot diverge. Populated from the
	// session ack's ga_display_name / ga_role / ga_description / ga_personality
	// (#478), loaded server-side from the v1:agents:agent record. Each is
	// optional: an ack that omits a field resolves it to "" and the instruction
	// builder renders the neutral default per field. Style carries the agent
	// record's personality prose.
	DisplayName string
	Role        string
	Description string
	Style       string
}

// AvatarEnabled reports whether this persona drives an avatar participant:
// a non-"none" vendor AND a stamped avatar persona id. Used by #460.
func (p Persona) AvatarEnabled() bool {
	return p.AvatarPersonaID != "" && p.AvatarVendor != "" && p.AvatarVendor != "none"
}

// AudioEnabled reports whether the GA should publish audio at session start:
// any mode other than "always_off". "mirror_user" is treated as enabled --
// the cascade/realtime loop decides per-track whether to actually publish.
func (p Persona) AudioEnabled() bool {
	return p.InitialAudioMode != "always_off"
}

// VideoEnabled reports whether the GA should publish video (avatar) at
// session start: any mode other than "always_off".
func (p Persona) VideoEnabled() bool {
	return p.InitialVideoMode != "always_off"
}

// ResolvePersona turns the resolved SessionAck plus the process Config into a
// Persona. It is the Go entry point analogous to
// persona_resolver.py::resolve_persona, minus the gRPC round-trip (session.go
// already performed it). Pure and deterministic so it is fully unit-testable.
//
// Resolution rules, matching the Python agent:
//   - canonical voice: ack value, or defaultCanonicalVoice ("alto") when unset;
//   - avatar persona id: ack value verbatim (empty is a valid "no avatar");
//   - avatar vendor: from Config (the ack does not carry it);
//   - audio/video modes: ack value, or defaultChannelMode ("mirror_user") when
//     unset, then video gating (see below).
//
// Video gating: a hard "always_off" audio mode forces video "always_off" too.
// You cannot have an always-on avatar video track while the agent's audio is
// hard-muted -- a talking head with no voice is worse than no avatar. This
// mirrors the avatar-gating intent the Python agent applies (ROADMAP
// "audioControl / videoControl ... avatar gating"): video never outlives
// audio. "mirror_user" audio does NOT gate video -- mirror means "follow the
// user", which the media loop resolves per-track at runtime.
func ResolvePersona(ack SessionAck, cfg Config) Persona {
	canonical := ack.CanonicalVoice
	if canonical == "" {
		canonical = defaultCanonicalVoice
	}

	audio := ack.InitialAudio
	if audio == "" {
		audio = defaultChannelMode
	}
	video := ack.InitialVideo
	if video == "" {
		video = defaultChannelMode
	}

	// Avatar gating: hard-off audio forces hard-off video.
	if audio == "always_off" {
		video = "always_off"
	}

	return Persona{
		CanonicalVoice:   canonical,
		AvatarPersonaID:  ack.AvatarPersonaID,
		AvatarVendor:     cfg.AvatarVendor,
		InitialAudioMode: audio,
		InitialVideoMode: video,
		// Persona-prompt fields from the ack (#478). Style maps from the
		// agent record's personality prose. Empty when the ack omits them;
		// the instruction builder renders the neutral default per field.
		DisplayName: ack.DisplayName,
		Role:        ack.Role,
		Description: ack.Description,
		Style:       ack.Personality,
	}
}
