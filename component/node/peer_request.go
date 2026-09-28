package node

import (
	"fmt"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// SendRequest hands one REQUEST-shaped envelope -- a forward whose sender
// parks on the answer -- to the named peer's CURRENT stream attempt, and
// returns a channel that closes when that attempt ends.
//
// A REQUEST THAT WAS NOT HANDED OVER IS AN ERROR. The general outbox
// (peerConnection.Send) is the right transport for heartbeats and gossip,
// which are fire-and-forget: it drops a message when it is full or closed and
// says so only in a log line, and between attempts it queues for a stream
// that does not exist yet. A request sent that way was reported as handed off
// either way, and its sender then waited out its own deadline -- minutes, for
// an app call -- for an answer that could not come. Refusing here lets the
// sender report "refused before start" and its route move on.
//
// THE RETURNED CHANNEL IS THE HOLDER-GONE SIGNAL. Every answer to this request
// comes back on the stream it went out on, so once that attempt ends no answer
// can arrive; a sender that selects on it fails fast instead of parking.
//
// It is the transport the AI forward already uses (AiForwardRouter.Forward),
// named here so the worker forwards can use it through the PeerManager
// without reaching into an unexported connection type.
func (pm *PeerManager) SendRequest(nodeId string, msg *nodev1.NodeClientMessage) (<-chan struct{}, error) {
	if pm == nil {
		return nil, fmt.Errorf("node: no peer manager to send through")
	}
	entry := pm.Get(nodeId)
	if entry == nil {
		return nil, fmt.Errorf("node: %s is not a known peer", nodeId)
	}
	conn, ok := pm.sendTarget(entry)
	if !ok {
		return nil, fmt.Errorf("node: this node holds no connection to %s", nodeId)
	}
	done, err := conn.SendOnStream(msg, nil)
	if err != nil {
		return nil, fmt.Errorf("node: %s: %w", nodeId, err)
	}
	return done, nil
}
