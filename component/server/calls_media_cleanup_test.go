package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

func TestMediaCleanupReceiptHasHumanAgentParity(t *testing.T) {
	h := &callsHandler{now: func() time.Time { return time.Unix(100, 0) }}
	status := &calls.MediaCleanupStatus{State: "pending", PendingSignals: 1, Reason: "handshake_outcome_unknown"}
	view := calls.SeatCall{MediaCleanup: status}
	human := h.sessionReply(calls.Session{}, view)
	agent := h.agentReply(view).(callsAgentSessionReply)
	require.Equal(t, status, human.MediaCleanup)
	require.Equal(t, human.MediaCleanup, agent.MediaCleanup)
	require.Nil(t, h.sessionReply(calls.Session{}, calls.SeatCall{}).MediaCleanup)
}
