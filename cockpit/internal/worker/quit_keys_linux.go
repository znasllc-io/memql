//go:build linux

package worker

import "golang.org/x/sys/unix"

const (
	ioctlTermiosGet = unix.TCGETS
	ioctlTermiosSet = unix.TCSETS
)
