// Package agent is the Go voice-agent: the media participant that joins a
// LiveKit room on behalf of a memQL space's General Assistant, opens a
// MemqlService.Stream gRPC session, and runs the turn-taking / STT / TTS
// orchestration that the retired Python voice-agent (LiveKit
// Agents 1.5) used to own.
//
// This file is the foundational skeleton landed for issue #454 (epic #449):
// config + env loading, the gRPC bidirectional-stream client, the session
// lifecycle, and the LiveKit room JOIN. The audio loop (STT/TTS, turn-taking,
// barge-in), persona/grounding parity, the realtime executor, MCP/output, the
// lifecycle guardrails, and the avatar are out of scope here and left as
// clearly-marked TODO seams referencing their follow-up issues (#455-#460).
//
// Build-tag discipline: everything in this package that imports the LiveKit
// server SDK or its CGO media-sdk (the libopus/soxr-bound PCM path) lives
// behind a `//go:build voice` tag so the default CGO-free CI lanes
// (`go build ./...`, `go vet ./...`, `go test ./...`) never pull CGO. The
// config, gRPC client, and session lifecycle here are pure-Go and tested in
// the default lane; the room-join wiring (room_voice.go) is voice-tagged.
package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the resolved runtime configuration for one voice-agent process.
// It mirrors the env-var family the Python config.py already defines so a
// deployment switching from the Python agent to the Go agent re-uses the same
// names verbatim. Read once at startup.
type Config struct {
	// LiveKit room transport.
	LiveKitURL       string
	LiveKitAPIKey    string
	LiveKitAPISecret string
	// LiveKitPublicURL is the externally-reachable LiveKit URL the avatar
	// vendor's cloud engine (#460) dials in on. The agent's own LiveKitURL is
	// usually an internal hostname (ws://livekit:7880), unreachable from
	// outside; when set this takes precedence for the avatar session-token
	// environment. Optional; falls back to LiveKitURL. Mirrors the Python
	// LIVEKIT_PUBLIC_URL handling in anam_persona_session.py.
	LiveKitPublicURLEnv string

	// Deepgram (STT + TTS). Carried here for #455; not consumed yet.
	DeepgramAPIKey string

	// Voice executor selection (#440 / #434). "cascade" (default) is the
	// Deepgram STT -> cognition -> Deepgram TTS path; "realtime" swaps in the
	// OpenAI gpt-realtime speech-to-speech model. Validated here so a typo
	// fails loudly at startup; the executor itself is #457.
	VoiceExecutor string // "cascade" | "realtime"
	OpenAIAPIKey  string // only required on the realtime path (#457)
	RealtimeModel string
	// RealtimeNativeTurn enables the #478 native 1-on-1 gate (semantic_vad +
	// native authorship for a single-human standard space). Default true.
	RealtimeNativeTurn bool
	// VoiceGrounding enables per-turn knowledge grounding for voice replies
	// (#490). When on, 1-on-1 routes through the gate (create_response:false) so
	// the executor can inject the retrieved grounding block before the model
	// generates -- otherwise native 1-on-1 (create_response:true) has no inject
	// window. Off by default; MUST match the cognition-side MEMQL_VOICE_GROUNDING
	// so retrieval and routing agree.
	VoiceGrounding bool
	// VoiceAutoJoin enables the dev auto-join dispatcher: when the voice-agent
	// is launched with no --room / MEMQL_VOICE_ROOM_NAME, it watches LiveKit for
	// active polyphon-<spaceId> rooms and joins one, so voice "just works" in
	// dev without launching a per-room process by hand. Default true; production
	// launches the agent per-room (--room), so it never hits this path. Set
	// MEMQL_VOICE_AUTOJOIN=false to force the plain idle behaviour instead.
	VoiceAutoJoin bool
	// RealtimeMultiPartySemanticVad enables the #481 multi-party gate:
	// semantic_vad turn detection + the conductor gate + native generation for
	// a >=2-human room (vs the turn_detection:null + Deepgram path). Default
	// false -- opt-in, pending live validation. Requires RealtimeNativeTurn.
	RealtimeMultiPartySemanticVad bool
	// RealtimeTranscriptionModel is the model id for the realtime session's
	// native input-audio transcription on the native path (#478).
	RealtimeTranscriptionModel string

	// Realtime lifecycle + cost guardrails (#439 / #459). Parsed here so the
	// env surface matches the Python agent; enforcement lands in #459.
	RealtimeIdleTimeoutSec int
	RealtimeMaxSessionSec  int
	RealtimeMaxAudioTokens int

	// memQL gRPC.
	MemqlGRPCAddr string
	// Identity-issued class="voice_agent" JWT bearer presented on every
	// MemqlService.Stream dial. Resolved by ResolveVoiceAgentToken (operator-
	// provisioned token, then the self-bootstrap path). The memQL voice-agent
	// stream interceptor verifies it via the cluster JWKS and admits it to the
	// VoiceAgent* message surface only.
	VoiceAgentToken string

	// Avatar (#460). Parsed + validated here; the avatar participant itself is
	// a follow-up.
	AvatarVendor         string // "anam" | "simli" | "none"
	AnamAPIKey           string
	SimliAPIKey          string
	AnamDefaultPersonaID string
	AnamDefaultAvatarID  string
	AnamDefaultPersonaNm string

	// Logging.
	LogLevel string

	// Deepgram tuning -- inherited from the Python defaults so the Go path
	// picks up the same EOU semantics. Consumed by the turn-taking machine
	// (#455), parsed here for parity.
	DGASRModel       string
	DGTTSModel       string
	DGLanguage       string
	DGEndpointingMs  int
	DGUtteranceEndMs int
}

// AvatarEnabled reports whether an avatar vendor is configured.
func (c Config) AvatarEnabled() bool {
	return c.AvatarVendor == "anam" || c.AvatarVendor == "simli"
}

// LiveKitPublicURL is the URL the avatar vendor's cloud engine should dial to
// join the room: LIVEKIT_PUBLIC_URL when set, else the agent's own LiveKitURL.
func (c Config) LiveKitPublicURL() string {
	if v := strings.TrimSpace(c.LiveKitPublicURLEnv); v != "" {
		return v
	}
	return c.LiveKitURL
}

// Getenv is the environment accessor LoadConfig reads through. Overridable in
// tests so config resolution can be exercised without touching the real
// process environment.
type Getenv func(key string) string

// LoadConfig resolves + validates the voice-agent configuration from the
// environment. It returns an error rather than panicking so the caller can
// surface a clean operator diagnostic. The token-resolution step (operator-
// provisioned vs self-bootstrap) is delegated to ResolveVoiceAgentToken.
//
// getenv may be nil, in which case os.Getenv is used.
func LoadConfig(getenv Getenv) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	get := func(key, def string) string {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return def
		}
		return v
	}
	getRequired := func(key string) (string, error) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return "", fmt.Errorf("required env var %s is unset", key)
		}
		return v, nil
	}
	getInt := func(key string, def int) int {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			// Match the Python agent: warn-and-fall-back rather than fail.
			return def
		}
		return n
	}

	var cfg Config
	var err error

	if cfg.LiveKitURL, err = getRequired("LIVEKIT_URL"); err != nil {
		return Config{}, err
	}
	if cfg.LiveKitAPIKey, err = getRequired("LIVEKIT_API_KEY"); err != nil {
		return Config{}, err
	}
	if cfg.LiveKitAPISecret, err = getRequired("LIVEKIT_API_SECRET"); err != nil {
		return Config{}, err
	}
	cfg.LiveKitPublicURLEnv = get("LIVEKIT_PUBLIC_URL", "")
	if cfg.DeepgramAPIKey, err = getRequired("MEMQL_DEEPGRAM_API_KEY"); err != nil {
		return Config{}, err
	}
	if cfg.MemqlGRPCAddr, err = getRequired("MEMQL_GRPC_ADDR"); err != nil {
		return Config{}, err
	}

	// Realtime (OpenAI gpt-realtime speech-to-speech) is the default executor
	// (#483): a fresh run uses the realtime path. It degrades cleanly back to
	// the cascade when its preconditions fail (missing OPENAI_API_KEY / persona
	// build) -- see SelectVoiceExecutor -- and the cascade stays available
	// explicitly via MEMQL_VOICE_EXECUTOR=cascade.
	cfg.VoiceExecutor = strings.ToLower(get("MEMQL_VOICE_EXECUTOR", "realtime"))
	cfg.OpenAIAPIKey = get("OPENAI_API_KEY", "")
	cfg.RealtimeModel = get("MEMQL_REALTIME_MODEL", "gpt-realtime")
	// #478 native 1-on-1: gpt-realtime owns the turn (semantic_vad) when a
	// standard space has exactly one human. On by default; set
	// MEMQL_REALTIME_NATIVE_TURN=false to keep the conductor gate on the
	// realtime path (a finer-grained rollback than dropping to the cascade).
	cfg.VoiceGrounding = get("MEMQL_VOICE_GROUNDING", "false") == "true"
	cfg.VoiceAutoJoin = get("MEMQL_VOICE_AUTOJOIN", "true") != "false"
	cfg.RealtimeNativeTurn = get("MEMQL_REALTIME_NATIVE_TURN", "true") != "false"
	cfg.RealtimeTranscriptionModel = get("MEMQL_REALTIME_TRANSCRIPTION_MODEL", "gpt-4o-mini-transcribe")
	// #481 multi-party semantic_vad gate -- opt-in (default false), pending live
	// validation in a >=2-human room.
	cfg.RealtimeMultiPartySemanticVad = get("MEMQL_REALTIME_MULTIPARTY_SEMANTIC_VAD", "false") == "true"
	cfg.RealtimeIdleTimeoutSec = getInt("MEMQL_REALTIME_IDLE_TIMEOUT_SEC", 300)
	cfg.RealtimeMaxSessionSec = getInt("MEMQL_REALTIME_MAX_SESSION_SEC", 1800)
	cfg.RealtimeMaxAudioTokens = getInt("MEMQL_REALTIME_MAX_AUDIO_TOKENS", 1_000_000)

	cfg.AvatarVendor = strings.ToLower(get("MEMQL_AVATAR_VENDOR", "anam"))
	cfg.AnamAPIKey = get("ANAM_API_KEY", "")
	cfg.SimliAPIKey = get("SIMLI_API_KEY", "")
	cfg.AnamDefaultPersonaID = get("ANAM_DEFAULT_PERSONA_ID", "")
	cfg.AnamDefaultAvatarID = get("ANAM_DEFAULT_AVATAR_ID", "")
	cfg.AnamDefaultPersonaNm = get("ANAM_DEFAULT_PERSONA_NAME", "Assistant")

	cfg.LogLevel = strings.ToUpper(get("VOICE_AGENT_LOG_LEVEL", "INFO"))

	cfg.DGASRModel = get("POLYPHON_DEEPGRAM_ASR_MODEL", "nova-3")
	cfg.DGTTSModel = get("POLYPHON_DEEPGRAM_TTS_MODEL", "aura-2")
	cfg.DGLanguage = get("POLYPHON_DEEPGRAM_LANGUAGE", "en")
	cfg.DGEndpointingMs = getInt("POLYPHON_DEEPGRAM_ENDPOINTING_MS", 2000)
	cfg.DGUtteranceEndMs = getInt("POLYPHON_DEEPGRAM_UTTERANCE_END_MS", 0)

	if cfg.VoiceExecutor != "cascade" && cfg.VoiceExecutor != "realtime" {
		return Config{}, fmt.Errorf(
			"MEMQL_VOICE_EXECUTOR=%q -- must be 'cascade' or 'realtime'", cfg.VoiceExecutor)
	}
	if cfg.AvatarVendor != "anam" && cfg.AvatarVendor != "simli" && cfg.AvatarVendor != "none" {
		return Config{}, fmt.Errorf(
			"MEMQL_AVATAR_VENDOR=%q -- must be 'anam', 'simli', or 'none'", cfg.AvatarVendor)
	}

	return cfg, nil
}
