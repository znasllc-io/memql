package worker

import (
	"errors"
	"io"

	"google.golang.org/grpc/status"
)

// DisconnectReasonServerDrain is the stable reason logged on both agent and
// cockpit when a voluntary node drain ends the WorkerService stream. Success
// criterion for rolls: one reconnect with this reason, not an unlabeled 408.
const DisconnectReasonServerDrain = "server_drain"

// disconnectCodeReason extracts a stable code + reason for disconnect logs.
// Prod tip windows had "worker disconnected" with no code/reason, which made
// roll-induced holder death indistinguishable from clean client close.
func disconnectCodeReason(cause error, drainReason string) (code, reason string) {
	if drainReason != "" {
		return "canceled", drainReason
	}
	if cause == nil {
		return "ok", "session_end"
	}
	if errors.Is(cause, io.EOF) {
		return "eof", "client_eof"
	}
	if st, ok := status.FromError(cause); ok {
		msg := st.Message()
		if msg == "" {
			msg = st.Code().String()
		}
		return st.Code().String(), msg
	}
	return "unknown", cause.Error()
}
