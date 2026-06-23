package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestLogVoiceTiming_EmitsDiscoverableKey pins the #1426 contract: every
// timing record carries the voice_timing key, a duration_ms, and the
// caller-supplied context attrs, so one grep finds the whole breakdown.
func TestLogVoiceTiming_EmitsDiscoverableKey(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	start := time.Now().Add(-1500 * time.Millisecond)
	logVoiceTiming(logger, "setup.realtime_connect", start, "partition_id", "space-1")

	out := buf.String()
	assert.Contains(t, out, "voice timing")
	assert.Contains(t, out, "voice_timing=setup.realtime_connect")
	assert.Contains(t, out, "duration_ms=")
	assert.Contains(t, out, "partition_id=space-1")

	// The measured duration reflects the start stamp (>= the 1.5s offset).
	idx := strings.Index(out, "duration_ms=")
	rest := out[idx+len("duration_ms="):]
	ms := rest[:strings.IndexAny(rest, " \n")]
	assert.GreaterOrEqual(t, len(ms), 4, "expected a >=1000ms duration, got %q", ms)
}

// TestLogVoiceTiming_NilLoggerSafe pins that instrumentation never panics on
// the nil-logger paths the executor and bridges tolerate everywhere else.
func TestLogVoiceTiming_NilLoggerSafe(t *testing.T) {
	assert.NotPanics(t, func() {
		logVoiceTiming(nil, "turn.gate_roundtrip", time.Now())
	})
}
