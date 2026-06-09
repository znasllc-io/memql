package agent

import "github.com/znasllc-io/memql/integrations/openai"

// realtimeServerVad builds the server_vad turn-detection config for the native
// 1-on-1 realtime path from the tunable #1203 knobs on Config.
//
// server_vad (not semantic_vad) is deliberate: semantic_vad stalled 6-15s before
// committing a finished turn here and broke barge-in, whereas server_vad is a
// deterministic energy gate -- it starts a turn when input crosses Threshold and
// commits SilenceDurationMs after it drops back below. Threshold is therefore the
// lever that stops the model RESPONDING to non-speech (#1203): a low-energy
// ambient-noise spike below the gate never becomes a turn, so the post-hoc
// transcript filter (#1199) is the belt and this energy gate is the suspenders.
//
// CreateResponse stays true (the model auto-authors on the native path) and
// InterruptResponse stays true (native barge-in). This is a pure mapping with no
// CGO/build-tag dependency, so the resolved config is unit-testable directly; the
// //go:build voice room glue in room_realtime_voice.go calls it.
func realtimeServerVad(cfg Config) *openai.TurnDetectionConfig {
	return &openai.TurnDetectionConfig{
		Type:              "server_vad",
		CreateResponse:    true,
		InterruptResponse: true,
		Threshold:         cfg.RealtimeVadThreshold,
		PrefixPaddingMs:   cfg.RealtimeVadPrefixPaddingMs,
		SilenceDurationMs: cfg.RealtimeVadSilenceDurationMs,
	}
}
