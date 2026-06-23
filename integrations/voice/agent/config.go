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

// defaultVoiceMaxRooms bounds the per-replica concurrent session pool when
// MEMQL_VOICE_MAX_ROOMS is unset (#1395). A single replica serving more than a
// handful of live realtime sessions is already an outlier; the cap keeps a
// runaway discovery (or a buggy room that re-appears) from spawning unbounded
// gRPC + LiveKit sessions.
const defaultVoiceMaxRooms = 8

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

	// Voice executor selection (#440 / #434). "realtime" (default) is the
	// OpenAI gpt-realtime speech-to-speech model; "cascade" is the OpenAI
	// ASR -> cognition -> OpenAI TTS path. Validated here so a typo fails
	// loudly at startup; the executor itself is #457.
	VoiceExecutor string // "cascade" | "realtime"
	OpenAIAPIKey  string // required: both executors run on OpenAI (#1355)
	RealtimeModel string
	// RealtimeNativeTurn enables the #478 native 1-on-1 gate (semantic_vad +
	// native authorship for a single-human standard space). Default true.
	RealtimeNativeTurn bool
	// RealtimeNativeSTT takes the separate ASR stream OFF the realtime path:
	// gpt-realtime owns STT (native input transcription), turn detection
	// (semantic_vad), the voice, and tool-calling -- no second transcription
	// stream sits on the critical path, so the conversation is as snappy as
	// the model allows. The human's chat transcript still arrives (from the
	// model's input_audio_transcription) but asynchronously, off the voice
	// path. The standalone OpenAI ASR stream stays a fully-wired fallback
	// (the cascade executor + multi-party labeled-transcript read side) --
	// this only disables it for the realtime executor. Default true.
	RealtimeNativeSTT bool
	// VoiceGrounding enables per-turn knowledge grounding for voice replies
	// (#490). When on, 1-on-1 routes through the gate (create_response:false) so
	// the executor can inject the retrieved grounding block before the model
	// generates -- otherwise native 1-on-1 (create_response:true) has no inject
	// window. Off by default; MUST match the cognition-side MEMQL_VOICE_GROUNDING
	// so retrieval and routing agree.
	VoiceGrounding bool
	// VoiceAutoJoin enables the dev auto-join dispatcher: when the voice-agent
	// is launched with no --room / MEMQL_VOICE_ROOM_NAME, it watches LiveKit for
	// active polyphon-<partitionId> rooms and joins one, so voice "just works" in
	// dev without launching a per-room process by hand. Default true; production
	// launches the agent per-room (--room), so it never hits this path. Set
	// MEMQL_VOICE_AUTOJOIN=false to force the plain idle behaviour instead.
	VoiceAutoJoin bool
	// VoiceMaxRooms bounds how many rooms a single voice-agent replica serves
	// concurrently (#1395). The auto-join dispatcher discovers every active
	// human-occupied room and serves each in its own isolated session, so two
	// users in different spaces both get the GA at once instead of the second
	// waiting for the first to idle out. Default 8; MEMQL_VOICE_MAX_ROOMS
	// overrides. A value <=0 falls back to the default.
	VoiceMaxRooms int
	// RealtimeMultiPartySemanticVad enables the #481 multi-party gate:
	// semantic_vad turn detection + the conductor gate + native generation for
	// a >=2-human room (vs the turn_detection:null + labeled-ASR path). Default
	// false -- opt-in, pending live validation. Requires RealtimeNativeTurn.
	RealtimeMultiPartySemanticVad bool
	// RealtimeTranscriptionModel is the model id for the realtime session's
	// native input-audio transcription on the native path (#478).
	RealtimeTranscriptionModel string
	// RealtimeAgentToolLoop routes every voice turn through cognition's full
	// agent tool loop instead of letting the realtime model author natively
	// (#1198, epic #1197 A2). When on, a 1-on-1 turn is forced through the gated
	// path (create_response:false) so it round-trips to cognition; cognition runs
	// the same tool loop as text chat (produceArtifact etc.) and the realtime
	// model re-voices the authored reply. Off by default -- opt-in via
	// MEMQL_VOICE_AGENT_TOOL_LOOP=true, pending live verification; flag off keeps
	// today's native authorship unchanged. MUST match the cognition-side flag.
	RealtimeAgentToolLoop bool

	// Realtime lifecycle + cost guardrails (#439 / #459). Parsed here so the
	// env surface matches the Python agent; enforcement lands in #459.
	RealtimeIdleTimeoutSec int
	RealtimeMaxSessionSec  int
	RealtimeMaxAudioTokens int

	// Non-blocking in-voice tool-call bounds (#1430, realtime_tools.go).
	// RealtimeToolTimeoutSec bounds one model-driven tool's memql CallTool
	// round-trip before a spoken-failure-style result is injected instead;
	// RealtimeMaxPendingTools caps concurrently-executing model-driven calls
	// (excess calls are answered with a busy error without executing).
	RealtimeToolTimeoutSec  int
	RealtimeMaxPendingTools int

	// Realtime server_vad turn-detection knobs (#1203) for the native 1-on-1
	// path. These tune the root-cause hallucination fix: silence/ambient noise
	// must not cross the energy gate and commit a phantom turn the model then
	// replies to. #1199 filters the bad transcript post-hoc; this stops the turn
	// being detected at all.
	//
	// RealtimeVadThreshold is the speech-energy gate (0..1). Raised from the
	// OpenAI 0.5 default to 0.6 so low-energy ambient noise no longer trips a
	// turn; tunable up toward 0.9 for noisy rooms. RealtimeVadPrefixPaddingMs is
	// how much audio before onset to keep; RealtimeVadSilenceDurationMs is how
	// long input must stay below threshold before the turn commits (end-of-turn
	// snappiness).
	RealtimeVadThreshold         float64
	RealtimeVadPrefixPaddingMs   int
	RealtimeVadSilenceDurationMs int

	// Realtime anti-phantom-transcript knobs (#1431), layered with the #1203
	// VAD threshold and the #1199 post-hoc filter.
	//
	// RealtimeNoiseReduction selects the GA audio.input.noise_reduction mode:
	// "far_field" (default -- laptop/conference mics, the documented mitigation
	// for speaker-echo phantom turns), "near_field" (headsets), or "off"
	// (field omitted). It filters input audio BEFORE the model's VAD, reducing
	// turn-detection false positives.
	RealtimeNoiseReduction string
	// RealtimeTranscriptMinConfidence is the mean per-token logprob floor an
	// input-transcription FINAL must clear to be kept (logprobs requested via
	// the session include option). Finals WITHOUT logprobs always pass --
	// the signal is intermittently missing, so absence must never drop a real
	// utterance. Default -1.0 (conservative: only clearly low-confidence,
	// noise-shaped finals fall below it); raise toward -0.5 to gate harder,
	// set very low (e.g. -100) to effectively disable.
	RealtimeTranscriptMinConfidence float64

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

	// Cascade / labeled-transcript ASR + TTS tuning (#1355). The cascade
	// executor and the realtime executor's optional labeled-transcript read
	// side both run on the OpenAI clients (integrations/openai); empty model
	// values fall back to the openai package defaults (whisper-1 ASR,
	// gpt-4o-mini-tts TTS).
	CascadeASRModel string
	CascadeTTSModel string
	// VoiceLanguage is the BCP-47 language for ASR sessions (e.g. "en",
	// "en-US"; OpenAI transcription wants the primary subtag).
	VoiceLanguage string
}

// AvatarEnabled reports whether an avatar vendor is configured.
func (c Config) AvatarEnabled() bool {
	return c.AvatarVendor == "anam" || c.AvatarVendor == "simli"
}

// NoiseReductionMode maps the validated MEMQL_REALTIME_NOISE_REDUCTION knob to
// the wire value for openai.SessionConfig.NoiseReduction: "far_field" /
// "near_field" pass through, "off" becomes the empty string (the encoder then
// omits audio.input.noise_reduction entirely) (#1431).
func (c Config) NoiseReductionMode() string {
	if c.RealtimeNoiseReduction == "off" {
		return ""
	}
	return c.RealtimeNoiseReduction
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
	getFloat := func(key string, def float64) float64 {
		raw := strings.TrimSpace(getenv(key))
		if raw == "" {
			return def
		}
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			// Warn-and-fall-back, matching getInt.
			return def
		}
		return f
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
	if cfg.MemqlGRPCAddr, err = getRequired("MEMQL_GRPC_ADDR"); err != nil {
		return Config{}, err
	}

	// Realtime (OpenAI gpt-realtime speech-to-speech) is the default executor
	// (#483): a fresh run uses the realtime path. It degrades cleanly back to
	// the cascade when its preconditions fail (persona build etc.) -- see
	// SelectVoiceExecutor -- and the cascade stays available explicitly via
	// MEMQL_VOICE_EXECUTOR=cascade. Both executors run on OpenAI (#1355), so
	// the key is required up front.
	cfg.VoiceExecutor = strings.ToLower(get("MEMQL_VOICE_EXECUTOR", "realtime"))
	if cfg.OpenAIAPIKey, err = getRequired("MEMQL_OPENAI_API_KEY"); err != nil {
		return Config{}, err
	}
	cfg.RealtimeModel = get("MEMQL_REALTIME_MODEL", "gpt-realtime-2")
	// #478 native 1-on-1: gpt-realtime owns the turn (semantic_vad) when a
	// standard space has exactly one human. On by default; set
	// MEMQL_REALTIME_NATIVE_TURN=false to keep the conductor gate on the
	// realtime path (a finer-grained rollback than dropping to the cascade).
	cfg.VoiceGrounding = get("MEMQL_VOICE_GROUNDING", "false") == "true"
	cfg.VoiceAutoJoin = get("MEMQL_VOICE_AUTOJOIN", "true") != "false"
	// #1395 concurrent multi-room serving: bound the per-replica session pool.
	if cfg.VoiceMaxRooms = getInt("MEMQL_VOICE_MAX_ROOMS", defaultVoiceMaxRooms); cfg.VoiceMaxRooms <= 0 {
		cfg.VoiceMaxRooms = defaultVoiceMaxRooms
	}
	cfg.RealtimeNativeTurn = get("MEMQL_REALTIME_NATIVE_TURN", "true") != "false"
	cfg.RealtimeNativeSTT = get("MEMQL_VOICE_REALTIME_NATIVE_STT", "true") != "false"
	cfg.RealtimeTranscriptionModel = get("MEMQL_REALTIME_TRANSCRIPTION_MODEL", "gpt-4o-mini-transcribe")
	// #481 multi-party semantic_vad gate -- opt-in (default false), pending live
	// validation in a >=2-human room.
	cfg.RealtimeMultiPartySemanticVad = get("MEMQL_REALTIME_MULTIPARTY_SEMANTIC_VAD", "false") == "true"
	// #1198 (epic #1197 A2): route voice through cognition's full agent tool loop.
	// Off by default; flip on only once both slices are verified live.
	cfg.RealtimeAgentToolLoop = get("MEMQL_VOICE_AGENT_TOOL_LOOP", "false") == "true"
	cfg.RealtimeIdleTimeoutSec = getInt("MEMQL_REALTIME_IDLE_TIMEOUT_SEC", 300)
	cfg.RealtimeMaxSessionSec = getInt("MEMQL_REALTIME_MAX_SESSION_SEC", 1800)
	cfg.RealtimeMaxAudioTokens = getInt("MEMQL_REALTIME_MAX_AUDIO_TOKENS", 1_000_000)
	// #1430 async tool-call bounds: per-call execution timeout + concurrent
	// pending-call cap for model-driven function calls.
	cfg.RealtimeToolTimeoutSec = getInt("MEMQL_REALTIME_TOOL_TIMEOUT_SEC", 45)
	cfg.RealtimeMaxPendingTools = getInt("MEMQL_REALTIME_MAX_PENDING_TOOLS", 4)
	// #1203 server_vad knobs. Defaults tightened over the OpenAI baseline
	// (threshold 0.5) so silence/noise no longer commits a phantom turn.
	cfg.RealtimeVadThreshold = getFloat("MEMQL_REALTIME_VAD_THRESHOLD", 0.6)
	cfg.RealtimeVadPrefixPaddingMs = getInt("MEMQL_REALTIME_VAD_PREFIX_PADDING_MS", 300)
	cfg.RealtimeVadSilenceDurationMs = getInt("MEMQL_REALTIME_VAD_SILENCE_DURATION_MS", 500)
	// #1431 anti-phantom-transcript knobs: server-side noise reduction
	// (default far_field -- our primary users are on laptop mics/speakers)
	// and the input-transcription confidence floor (mean token logprob).
	cfg.RealtimeNoiseReduction = strings.ToLower(get("MEMQL_REALTIME_NOISE_REDUCTION", "far_field"))
	cfg.RealtimeTranscriptMinConfidence = getFloat("MEMQL_REALTIME_TRANSCRIPT_MIN_CONFIDENCE", -1.0)

	cfg.AvatarVendor = strings.ToLower(get("MEMQL_AVATAR_VENDOR", "anam"))
	cfg.AnamAPIKey = get("MEMQL_ANAM_API_KEY", "")
	cfg.SimliAPIKey = get("MEMQL_SIMLI_API_KEY", "")
	cfg.AnamDefaultPersonaID = get("ANAM_DEFAULT_PERSONA_ID", "")
	cfg.AnamDefaultAvatarID = get("ANAM_DEFAULT_AVATAR_ID", "")
	cfg.AnamDefaultPersonaNm = get("ANAM_DEFAULT_PERSONA_NAME", "Assistant")

	cfg.LogLevel = strings.ToUpper(get("VOICE_AGENT_LOG_LEVEL", "INFO"))

	// Cascade / labeled-transcript clients (#1355). Empty model values fall
	// back to the openai package defaults.
	cfg.CascadeASRModel = get("MEMQL_POLYPHON_OPENAI_ASR_MODEL", "")
	cfg.CascadeTTSModel = get("MEMQL_POLYPHON_OPENAI_TTS_MODEL", "")
	cfg.VoiceLanguage = get("POLYPHON_VOICE_LANGUAGE", "en")

	if cfg.VoiceExecutor != "cascade" && cfg.VoiceExecutor != "realtime" {
		return Config{}, fmt.Errorf(
			"MEMQL_VOICE_EXECUTOR=%q -- must be 'cascade' or 'realtime'", cfg.VoiceExecutor)
	}
	if cfg.AvatarVendor != "anam" && cfg.AvatarVendor != "simli" && cfg.AvatarVendor != "none" {
		return Config{}, fmt.Errorf(
			"MEMQL_AVATAR_VENDOR=%q -- must be 'anam', 'simli', or 'none'", cfg.AvatarVendor)
	}
	if cfg.RealtimeNoiseReduction != "far_field" && cfg.RealtimeNoiseReduction != "near_field" &&
		cfg.RealtimeNoiseReduction != "off" {
		return Config{}, fmt.Errorf(
			"MEMQL_REALTIME_NOISE_REDUCTION=%q -- must be 'far_field', 'near_field', or 'off'",
			cfg.RealtimeNoiseReduction)
	}

	return cfg, nil
}
