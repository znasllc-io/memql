package worker

import (
	"errors"
	"time"
)

const heartbeatSendTimeout = time.Minute

// Transport keepalive detects a missing peer, but a peer that ACKs transport
// pings while no longer consuming this stream can block Send on flow control
// indefinitely. End that exact stream after a minute with no completed send.
// There is no receive-idle timeout: idle workers still send healthy heartbeats.
func (r *Runner) boundedHeartbeat(conn *Connection, send func() error) error {
	done := make(chan struct{})
	timedOut := false
	expire := func() {
		defer close(done)
		timedOut = true
		conn.Close()
	}
	var stop func() bool
	if r.heartbeatDeadline != nil {
		stop = r.heartbeatDeadline(heartbeatSendTimeout, expire)
	} else {
		stop = time.AfterFunc(heartbeatSendTimeout, expire).Stop
	}
	err := send()
	if !stop() {
		<-done
	}
	if timedOut {
		return errors.New("heartbeat send made no progress for one minute")
	}
	return err
}
