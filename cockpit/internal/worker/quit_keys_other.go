//go:build !darwin && !linux

package worker

import (
	"context"
	"log/slog"
)

// enableQuitHotkeys is a no-op on platforms without termios. Quit
// remains a SIGINT-only operation; the caller's signal.Notify
// chain handles it. Windows console hotkey support could land
// here later if needed.
func enableQuitHotkeys(_ context.Context, _ context.CancelFunc, _ *slog.Logger) func() {
	return func() {}
}
