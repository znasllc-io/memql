package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRun_NoRoomIdlesUntilCancelled verifies the idle-crash fix: with no room
// configured (the long-running-service-at-idle case), Run does NOT error and
// exit -- which docker turned into a crash-loop -- but idles until the context
// is cancelled, then shuts down cleanly. Token resolution is skipped while idle,
// so the test needs no network.
func TestRun_NoRoomIdlesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Getenv: envMap(baseEnv()), // required config present, but NO room
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	select {
	case err := <-done:
		assert.NoError(t, err, "no room -> idle then clean shutdown on cancel, not an error")
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the context was cancelled (it should idle, then exit)")
	}
}
