package worker

import (
	"errors"
	"io"
	"strings"

	"google.golang.org/grpc/status"
)

// DisconnectReasonServerDrain matches the agent-side stable reason for a
// voluntary node drain (memql component/worker.DisconnectReasonServerDrain).
const DisconnectReasonServerDrain = "server_drain"

var errServerDrain = errors.New("server requested drain")

// streamEndCodeReason extracts code + reason for "worker stream ended" logs.
// Tip windows showed nginx 200 clean closes with no reason on the cockpit;
// rolls must log server_drain so operators can tell drain from idle 408.
func streamEndCodeReason(err error) (code, reason string) {
	if err == nil {
		return "ok", "session_end"
	}
	if errors.Is(err, errServerDrain) || strings.Contains(err.Error(), "server requested drain") {
		return "canceled", DisconnectReasonServerDrain
	}
	if errors.Is(err, io.EOF) {
		return "eof", "client_eof"
	}
	if st, ok := status.FromError(err); ok {
		msg := st.Message()
		if msg == "" {
			msg = st.Code().String()
		}
		return st.Code().String(), msg
	}
	return "unknown", err.Error()
}
