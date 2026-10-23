//go:build darwin || linux

package worker

import (
	"context"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"
)

// enableQuitHotkeys puts stdin in non-canonical mode (single-byte
// reads, no echo) and disables IXON so Ctrl+Q (^Q, 0x11) reaches
// the process as a regular byte instead of being intercepted by
// the terminal driver as the XON flow-control character. Reads
// stdin one byte at a time and calls `cancel` when Ctrl+Q is
// pressed.
//
// ISIG is left ON so Ctrl+C still generates SIGINT through the
// existing signal.Notify path -- two independent quit triggers,
// no behavioral overlap.
//
// Returns a restore function the caller defers. The function is
// a no-op when stdin isn't a TTY (piped input, daemonized
// LaunchAgent), so daemon contexts behave exactly like before.
func enableQuitHotkeys(ctx context.Context, cancel context.CancelFunc, logger *slog.Logger) func() {
	fd := int(os.Stdin.Fd())
	orig, err := unix.IoctlGetTermios(fd, ioctlTermiosGet)
	if err != nil {
		// Stdin isn't a controlling TTY (piped / launchd / etc.).
		// SIGINT/SIGTERM remain the only quit triggers; no error.
		return func() {}
	}
	modified := *orig
	modified.Lflag &^= unix.ICANON | unix.ECHO
	modified.Iflag &^= unix.IXON
	if err := unix.IoctlSetTermios(fd, ioctlTermiosSet, &modified); err != nil {
		return func() {}
	}

	restore := func() {
		_ = unix.IoctlSetTermios(fd, ioctlTermiosSet, orig)
	}

	go func() {
		buf := make([]byte, 1)
		for {
			if ctx.Err() != nil {
				return
			}
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			if n == 0 {
				continue
			}
			if buf[0] == 0x11 { // Ctrl+Q (^Q, 0x11)
				if logger != nil {
					logger.Info("worker shutting down", "key", "Ctrl+Q")
				}
				cancel()
				return
			}
		}
	}()

	return restore
}
