package agent

// timing.go is the voice-path timing instrumentation helper (#1426). Every
// timed phase on the realtime voice path -- session setup, per-turn latency,
// tool round-trips, and each individual memQL/DB/mesh call inside those spans
// -- is logged through logVoiceTiming so one grep key (`voice_timing`) finds
// the whole breakdown:
//
//	kubectl logs ... | grep voice_timing
//
// Each record carries `voice_timing=<phase>` + `duration_ms=<elapsed>` plus
// caller-supplied context (space id, payload name, tool name, ...). This is
// measurement only: it changes no behavior and adds no synchronous work
// beyond the log call itself.

import (
	"log/slog"
	"time"
)

// voiceTimingKey is the discoverable structured-log key every voice-path
// timing record carries (#1426).
const voiceTimingKey = "voice_timing"

// logVoiceTiming emits one timing record for a completed phase that began at
// start. phase names the span (e.g. "setup.realtime_connect",
// "turn.gate_roundtrip", "tool.execute"); attrs append caller context as
// alternating key/value pairs, matching slog conventions. Nil-logger safe.
func logVoiceTiming(logger *slog.Logger, phase string, start time.Time, attrs ...any) {
	if logger == nil {
		return
	}
	args := make([]any, 0, len(attrs)+4)
	args = append(args, voiceTimingKey, phase, "duration_ms", time.Since(start).Milliseconds())
	args = append(args, attrs...)
	logger.Info("voice timing", args...)
}
