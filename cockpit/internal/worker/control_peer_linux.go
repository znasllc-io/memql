//go:build linux

package worker

import (
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func controlPeerIsCurrentUser(conn net.Conn) bool {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return false
	}
	valid := false
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		valid = e == nil && cred.Uid == uint32(os.Getuid())
	})
	return err == nil && valid
}
