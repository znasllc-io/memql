package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// envMap returns a Getenv backed by a map -- isolates config tests from the
// real process environment.
func envMap(m map[string]string) Getenv {
	return func(k string) string { return m[k] }
}

func baseEnv() map[string]string {
	return map[string]string{
		"LIVEKIT_URL":            "ws://livekit:7880",
		"LIVEKIT_API_KEY":        "key",
		"LIVEKIT_API_SECRET":     "secret",
		"OPENAI_API_KEY":         "sk-test",
		"MEMQL_GRPC_ADDR":        "memql:50051",
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv()))
	require.NoError(t, err)
	assert.Equal(t, "ws://livekit:7880", cfg.LiveKitURL)
	assert.Equal(t, "realtime", cfg.VoiceExecutor)
	assert.Equal(t, "gpt-realtime-2", cfg.RealtimeModel)
	// #478 native 1-on-1 is on by default with a sensible transcription model.
	assert.True(t, cfg.RealtimeNativeTurn)
	// Native STT (no second ASR stream on the realtime critical path) is on by default.
	assert.True(t, cfg.RealtimeNativeSTT)
	assert.Equal(t, "gpt-4o-mini-transcribe", cfg.RealtimeTranscriptionModel)
	assert.Equal(t, 300, cfg.RealtimeIdleTimeoutSec)
	assert.Equal(t, 1800, cfg.RealtimeMaxSessionSec)
	assert.Equal(t, 1_000_000, cfg.RealtimeMaxAudioTokens)
	// #1203 server_vad knobs default tightened over the OpenAI 0.5 baseline so
	// noise/silence does not commit a phantom turn.
	assert.Equal(t, 0.6, cfg.RealtimeVadThreshold)
	assert.Equal(t, 300, cfg.RealtimeVadPrefixPaddingMs)
	assert.Equal(t, 500, cfg.RealtimeVadSilenceDurationMs)
	// #1431 anti-phantom-transcript defaults: far_field noise reduction (our
	// users are on laptop mics) + a conservative mean-logprob floor.
	assert.Equal(t, "far_field", cfg.RealtimeNoiseReduction)
	assert.Equal(t, "far_field", cfg.NoiseReductionMode())
	assert.Equal(t, -1.0, cfg.RealtimeTranscriptMinConfidence)
	assert.Equal(t, "anam", cfg.AvatarVendor)
	assert.True(t, cfg.AvatarEnabled())
	// Cascade ASR/TTS models default empty -- the openai package applies
	// its own defaults (whisper-1 / gpt-4o-mini-tts) at client build time.
	assert.Equal(t, "", cfg.CascadeASRModel)
	assert.Equal(t, "", cfg.CascadeTTSModel)
	assert.Equal(t, "en", cfg.VoiceLanguage)
	assert.Equal(t, "INFO", cfg.LogLevel)
}

func TestLoadConfig_MissingRequired(t *testing.T) {
	for _, key := range []string{
		"LIVEKIT_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET",
		"OPENAI_API_KEY", "MEMQL_GRPC_ADDR",
	} {
		env := baseEnv()
		delete(env, key)
		_, err := LoadConfig(envMap(env))
		require.Error(t, err, "expected error when %s unset", key)
		assert.Contains(t, err.Error(), key)
	}
}

func TestLoadConfig_InvalidExecutor(t *testing.T) {
	env := baseEnv()
	env["MEMQL_VOICE_EXECUTOR"] = "magic"
	_, err := LoadConfig(envMap(env))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MEMQL_VOICE_EXECUTOR")
}

func TestLoadConfig_InvalidAvatarVendor(t *testing.T) {
	env := baseEnv()
	env["MEMQL_AVATAR_VENDOR"] = "holodeck"
	_, err := LoadConfig(envMap(env))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MEMQL_AVATAR_VENDOR")
}

func TestLoadConfig_RealtimeAndOverrides(t *testing.T) {
	env := baseEnv()
	env["MEMQL_VOICE_EXECUTOR"] = "realtime"
	env["MEMQL_VOICE_AGENT_INSTANCE_ID"] = "va-1"
	env["POLYPHON_OPENAI_ASR_MODEL"] = "gpt-4o-mini-transcribe"
	env["POLYPHON_OPENAI_TTS_MODEL"] = "gpt-4o-tts"
	env["MEMQL_AVATAR_VENDOR"] = "none"
	env["VOICE_AGENT_LOG_LEVEL"] = "debug"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, "realtime", cfg.VoiceExecutor)
	assert.Equal(t, "sk-test", cfg.OpenAIAPIKey)
	assert.Equal(t, "gpt-4o-mini-transcribe", cfg.CascadeASRModel)
	assert.Equal(t, "gpt-4o-tts", cfg.CascadeTTSModel)
	assert.Equal(t, "none", cfg.AvatarVendor)
	assert.False(t, cfg.AvatarEnabled())
	assert.Equal(t, "DEBUG", cfg.LogLevel)
}

// TestLoadConfig_CascadeOptOut: realtime is the default (#483), so cascade
// must be selectable explicitly as the opt-out.
func TestLoadConfig_CascadeOptOut(t *testing.T) {
	env := baseEnv()
	env["MEMQL_VOICE_EXECUTOR"] = "cascade"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, "cascade", cfg.VoiceExecutor)
}

// TestLoadConfig_NativeTurnOptOut: native 1-on-1 is on by default but can be
// disabled (the finer-grained realtime rollback per #478).
func TestLoadConfig_VoiceGroundingOptIn(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv()))
	require.NoError(t, err)
	assert.False(t, cfg.VoiceGrounding, "grounding defaults off")
	env := baseEnv()
	env["MEMQL_VOICE_GROUNDING"] = "true"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.True(t, cfg.VoiceGrounding)
}

func TestLoadConfig_VoiceAutoJoinDefaultAndOptOut(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv()))
	require.NoError(t, err)
	assert.True(t, cfg.VoiceAutoJoin, "auto-join defaults on")

	env := baseEnv()
	env["MEMQL_VOICE_AUTOJOIN"] = "false"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.False(t, cfg.VoiceAutoJoin)
}

func TestLoadConfig_VoiceMaxRoomsDefaultAndOverride(t *testing.T) {
	// #1395 concurrent multi-room serving: default cap, env override, and a
	// non-positive override falls back to the default.
	cfg, err := LoadConfig(envMap(baseEnv()))
	require.NoError(t, err)
	assert.Equal(t, defaultVoiceMaxRooms, cfg.VoiceMaxRooms, "defaults to the pool cap")

	env := baseEnv()
	env["MEMQL_VOICE_MAX_ROOMS"] = "3"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, 3, cfg.VoiceMaxRooms)

	env = baseEnv()
	env["MEMQL_VOICE_MAX_ROOMS"] = "0"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, defaultVoiceMaxRooms, cfg.VoiceMaxRooms, "non-positive falls back to default")
}

func TestLoadConfig_MultiPartySemanticVadOptIn(t *testing.T) {
	// #481 multi-party semantic_vad is off by default, opt-in via env.
	cfg, err := LoadConfig(envMap(baseEnv()))
	require.NoError(t, err)
	assert.False(t, cfg.RealtimeMultiPartySemanticVad)

	env := baseEnv()
	env["MEMQL_REALTIME_MULTIPARTY_SEMANTIC_VAD"] = "true"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.True(t, cfg.RealtimeMultiPartySemanticVad)
}

func TestLoadConfig_NativeTurnOptOut(t *testing.T) {
	env := baseEnv()
	env["MEMQL_REALTIME_NATIVE_TURN"] = "false"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.False(t, cfg.RealtimeNativeTurn)
}

// TestLoadConfig_AgentToolLoopOptIn: the #1198 (A2) flag is off by default
// (today's native authorship) and opt-in via MEMQL_VOICE_AGENT_TOOL_LOOP.
func TestLoadConfig_AgentToolLoopOptIn(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv()))
	require.NoError(t, err)
	assert.False(t, cfg.RealtimeAgentToolLoop, "A2 defaults off")

	env := baseEnv()
	env["MEMQL_VOICE_AGENT_TOOL_LOOP"] = "true"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.True(t, cfg.RealtimeAgentToolLoop)
}

func TestLoadConfig_IntFallbackOnGarbage(t *testing.T) {
	env := baseEnv()
	env["MEMQL_REALTIME_MAX_SESSION_SEC"] = "not-a-number"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, 1800, cfg.RealtimeMaxSessionSec) // falls back to default
}

// TestLoadConfig_VadOverrides: the #1203 server_vad knobs are env-tunable so a
// noisy room can raise the energy gate further without a rebuild.
func TestLoadConfig_VadOverrides(t *testing.T) {
	env := baseEnv()
	env["MEMQL_REALTIME_VAD_THRESHOLD"] = "0.8"
	env["MEMQL_REALTIME_VAD_PREFIX_PADDING_MS"] = "200"
	env["MEMQL_REALTIME_VAD_SILENCE_DURATION_MS"] = "700"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, 0.8, cfg.RealtimeVadThreshold)
	assert.Equal(t, 200, cfg.RealtimeVadPrefixPaddingMs)
	assert.Equal(t, 700, cfg.RealtimeVadSilenceDurationMs)
}

// TestLoadConfig_FloatFallbackOnGarbage: a non-numeric threshold falls back to
// the tightened default rather than failing the session.
func TestLoadConfig_FloatFallbackOnGarbage(t *testing.T) {
	env := baseEnv()
	env["MEMQL_REALTIME_VAD_THRESHOLD"] = "loud"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, 0.6, cfg.RealtimeVadThreshold) // falls back to default
}

// TestLoadConfig_NoiseReduction: the #1431 knob accepts the three documented
// modes (case-insensitively), maps "off" to the empty wire value (field
// omitted), and fails loudly on a typo so a misconfigured session never runs
// silently without the filter it claims to have.
func TestLoadConfig_NoiseReduction(t *testing.T) {
	env := baseEnv()
	env["MEMQL_REALTIME_NOISE_REDUCTION"] = "NEAR_FIELD"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, "near_field", cfg.RealtimeNoiseReduction)
	assert.Equal(t, "near_field", cfg.NoiseReductionMode())

	env["MEMQL_REALTIME_NOISE_REDUCTION"] = "off"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, "off", cfg.RealtimeNoiseReduction)
	assert.Equal(t, "", cfg.NoiseReductionMode(), "off maps to the empty wire value (field omitted)")

	env["MEMQL_REALTIME_NOISE_REDUCTION"] = "farfield"
	_, err = LoadConfig(envMap(env))
	require.Error(t, err, "a typo'd mode must fail at startup, not silently disable the filter")
	assert.Contains(t, err.Error(), "MEMQL_REALTIME_NOISE_REDUCTION")
}

// TestLoadConfig_TranscriptMinConfidenceOverride: the #1431 logprob floor is
// env-tunable (and a garbage value falls back to the conservative default).
func TestLoadConfig_TranscriptMinConfidenceOverride(t *testing.T) {
	env := baseEnv()
	env["MEMQL_REALTIME_TRANSCRIPT_MIN_CONFIDENCE"] = "-0.5"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, -0.5, cfg.RealtimeTranscriptMinConfidence)

	env["MEMQL_REALTIME_TRANSCRIPT_MIN_CONFIDENCE"] = "very confident"
	cfg, err = LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, -1.0, cfg.RealtimeTranscriptMinConfidence)
}
