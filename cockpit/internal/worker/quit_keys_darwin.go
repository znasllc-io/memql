//go:build darwin

package worker

import "golang.org/x/sys/unix"

// macOS termios ioctl request numbers. Linux uses TCGETS / TCSETS;
// the two are split into platform files because golang.org/x/sys
// declares them on each platform but with different identifiers.
const (
	ioctlTermiosGet = unix.TIOCGETA
	ioctlTermiosSet = unix.TIOCSETA
)
