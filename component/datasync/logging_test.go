package datasync

import (
	"io"
	"log/slog"
)

// discardLogger keeps a test's output about the assertions rather than
// about the runtime's ordinary progress lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
