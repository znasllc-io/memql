//go:build darwin || linux

package transfer

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentity(i os.FileInfo) string {
	if s, ok := i.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", s.Dev, s.Ino)
	}
	return ""
}
