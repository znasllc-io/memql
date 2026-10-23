//go:build !darwin && !linux

package worker

import "net"

func controlPeerIsCurrentUser(conn net.Conn) bool { return false }
