package memql

import (
	"bytes"
	"log/slog"
	"testing"
	"time"
)

// TestLogVoiceTiming_EmitsDiscoverableKey pins the #1426 contract on the
// server side: every voice-path timing record carries the voice_timing key,
// a duration_ms, and the caller context, mirroring the voice-agent helper so
// one grep yields the full client+server breakdown.
func TestLogVoiceTiming_EmitsDiscoverableKey(t *testing.T) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	logVoiceTiming(logger, "server.session_start.persona", time.Now(), "space_id", "space-1")

	out := buf.String()
	for _, want := range []string{
		"voice timing",
		"voice_timing=server.session_start.persona",
		"duration_ms=",
		"space_id=space-1",
	} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("timing record missing %q; got %q", want, out)
		}
	}
}

// TestLogVoiceTiming_NilLoggerSafe pins that the instrumentation never panics
// on nil-logger paths.
func TestLogVoiceTiming_NilLoggerSafe(t *testing.T) {
	logVoiceTiming(nil, "server.turn.no_reply", time.Now())
}
