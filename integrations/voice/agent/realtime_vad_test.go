package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRealtimeServerVad_MapsKnobs: the native-path turn detector is server_vad
// with auto-author + barge-in, and carries the tunable #1203 energy-gate knobs
// straight off Config so the resolved session config is asserted CGO-free
// (room_realtime_voice.go that calls it is //go:build voice).
func TestRealtimeServerVad_MapsKnobs(t *testing.T) {
	td := realtimeServerVad(Config{
		RealtimeVadThreshold:         0.6,
		RealtimeVadPrefixPaddingMs:   300,
		RealtimeVadSilenceDurationMs: 500,
	})
	assert.Equal(t, "server_vad", td.Type)
	// Native 1-on-1: the model auto-authors and barge-in is native.
	assert.True(t, td.CreateResponse)
	assert.True(t, td.InterruptResponse)
	// The raised energy gate is the root-cause noise-rejection lever (#1203).
	assert.Equal(t, 0.6, td.Threshold)
	assert.Equal(t, 300, td.PrefixPaddingMs)
	assert.Equal(t, 500, td.SilenceDurationMs)
	// semantic_vad's eagerness knob must not leak onto a server_vad config.
	assert.Empty(t, td.Eagerness)
}

// TestRealtimeServerVad_HonorsTunedThreshold: a noisy-room override flows
// through the helper untouched.
func TestRealtimeServerVad_HonorsTunedThreshold(t *testing.T) {
	td := realtimeServerVad(Config{
		RealtimeVadThreshold:         0.85,
		RealtimeVadPrefixPaddingMs:   200,
		RealtimeVadSilenceDurationMs: 700,
	})
	assert.Equal(t, 0.85, td.Threshold)
	assert.Equal(t, 200, td.PrefixPaddingMs)
	assert.Equal(t, 700, td.SilenceDurationMs)
}
