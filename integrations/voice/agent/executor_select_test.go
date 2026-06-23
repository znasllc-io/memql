package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSelectVoiceExecutor_CascadeSelected verifies that anything other than
// "realtime" resolves to the cascade with no fallback reason, and the realtime
// build check is never consulted. (The LoadConfig-level default is now
// realtime per #483 -- see config_test; this guards the selection function's
// non-realtime branch, which also covers a zero-value Config.)
func TestSelectVoiceExecutor_CascadeSelected(t *testing.T) {
	called := false
	restore := swapRealtimeBuildCheck(func(Config, Persona) (SessionPersona, error) {
		called = true
		return SessionPersona{}, nil
	})
	defer restore()

	for _, executor := range []string{"", "cascade", "CASCADE", "  cascade  "} {
		plan := SelectVoiceExecutor(Config{VoiceExecutor: executor}, Persona{}, nil)
		assert.Equal(t, ExecutorCascade, plan.Kind, "executor=%q", executor)
		assert.Empty(t, plan.FallbackReason)
		assert.False(t, plan.IsRealtime())
	}
	assert.False(t, called, "realtime build check must not run on the cascade path")
}

// TestSelectVoiceExecutor_RealtimeSelected verifies that a clean realtime
// selection returns the realtime plan carrying the resolved session persona.
func TestSelectVoiceExecutor_RealtimeSelected(t *testing.T) {
	restore := swapRealtimeBuildCheck(func(Config, Persona) (SessionPersona, error) {
		return SessionPersona{Instructions: "be helpful", Voice: "marin"}, nil
	})
	defer restore()

	plan := SelectVoiceExecutor(Config{VoiceExecutor: "realtime"}, Persona{}, nil)
	assert.Equal(t, ExecutorRealtime, plan.Kind)
	assert.True(t, plan.IsRealtime())
	assert.Empty(t, plan.FallbackReason)
	assert.Equal(t, "marin", plan.SessionPersona.Voice)
	assert.Equal(t, "be helpful", plan.SessionPersona.Instructions)
}

// TestSelectVoiceExecutor_FallbackOnError verifies the clean fallback: when the
// realtime build check fails (e.g. missing key / build error), the plan falls
// back to the cascade with the failure recorded as the fallback reason -- the
// voice path always comes up.
func TestSelectVoiceExecutor_FallbackOnError(t *testing.T) {
	restore := swapRealtimeBuildCheck(func(Config, Persona) (SessionPersona, error) {
		return SessionPersona{}, newRealtimeExecutorError("boom: %s", "no key")
	})
	defer restore()

	plan := SelectVoiceExecutor(Config{VoiceExecutor: "realtime"}, Persona{}, nil)
	assert.Equal(t, ExecutorCascade, plan.Kind, "must fall back to cascade")
	assert.False(t, plan.IsRealtime())
	assert.Equal(t, "boom: no key", plan.FallbackReason)
}

// TestSelectVoiceExecutor_FallbackOnMissingKey exercises the real build check
// (not the test hook): realtime requested with no MEMQL_OPENAI_API_KEY falls back.
func TestSelectVoiceExecutor_FallbackOnMissingKey(t *testing.T) {
	plan := SelectVoiceExecutor(Config{VoiceExecutor: "realtime", OpenAIAPIKey: ""}, Persona{}, nil)
	assert.Equal(t, ExecutorCascade, plan.Kind)
	assert.Contains(t, plan.FallbackReason, "MEMQL_OPENAI_API_KEY is unset")
}

// TestValidateRealtimeBuildable_OK verifies the real check passes with a key
// and resolves a non-empty session persona from the catalog default.
func TestValidateRealtimeBuildable_OK(t *testing.T) {
	sp, err := validateRealtimeBuildable(Config{OpenAIAPIKey: "sk-test"}, Persona{CanonicalVoice: "alto"})
	assert.NoError(t, err)
	assert.NotEmpty(t, sp.Instructions)
	assert.NotEmpty(t, sp.Voice)
}

// swapRealtimeBuildCheck temporarily replaces the package-level build-check
// hook and returns a restore func.
func swapRealtimeBuildCheck(fn func(Config, Persona) (SessionPersona, error)) func() {
	prev := realtimeBuildCheck
	realtimeBuildCheck = fn
	return func() { realtimeBuildCheck = prev }
}
