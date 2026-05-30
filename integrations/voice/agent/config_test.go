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
		"MEMQL_DEEPGRAM_API_KEY": "dg",
		"MEMQL_GRPC_ADDR":        "memql:50051",
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig(envMap(baseEnv()))
	require.NoError(t, err)
	assert.Equal(t, "ws://livekit:7880", cfg.LiveKitURL)
	assert.Equal(t, "cascade", cfg.VoiceExecutor)
	assert.Equal(t, "gpt-realtime", cfg.RealtimeModel)
	assert.Equal(t, 300, cfg.RealtimeIdleTimeoutSec)
	assert.Equal(t, 1800, cfg.RealtimeMaxSessionSec)
	assert.Equal(t, 1_000_000, cfg.RealtimeMaxAudioTokens)
	assert.Equal(t, "anam", cfg.AvatarVendor)
	assert.True(t, cfg.AvatarEnabled())
	assert.Equal(t, "nova-3", cfg.DGASRModel)
	assert.Equal(t, "aura-2", cfg.DGTTSModel)
	assert.Equal(t, 2000, cfg.DGEndpointingMs)
	assert.Equal(t, "INFO", cfg.LogLevel)
}

func TestLoadConfig_MissingRequired(t *testing.T) {
	for _, key := range []string{
		"LIVEKIT_URL", "LIVEKIT_API_KEY", "LIVEKIT_API_SECRET",
		"MEMQL_DEEPGRAM_API_KEY", "MEMQL_GRPC_ADDR",
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
	env["OPENAI_API_KEY"] = "sk-test"
	env["MEMQL_VOICE_AGENT_INSTANCE_ID"] = "va-1"
	env["POLYPHON_DEEPGRAM_ENDPOINTING_MS"] = "1500"
	env["MEMQL_AVATAR_VENDOR"] = "none"
	env["VOICE_AGENT_LOG_LEVEL"] = "debug"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, "realtime", cfg.VoiceExecutor)
	assert.Equal(t, "sk-test", cfg.OpenAIAPIKey)
	assert.Equal(t, 1500, cfg.DGEndpointingMs)
	assert.Equal(t, "none", cfg.AvatarVendor)
	assert.False(t, cfg.AvatarEnabled())
	assert.Equal(t, "DEBUG", cfg.LogLevel)
}

func TestLoadConfig_IntFallbackOnGarbage(t *testing.T) {
	env := baseEnv()
	env["MEMQL_REALTIME_MAX_SESSION_SEC"] = "not-a-number"
	cfg, err := LoadConfig(envMap(env))
	require.NoError(t, err)
	assert.Equal(t, 1800, cfg.RealtimeMaxSessionSec) // falls back to default
}
