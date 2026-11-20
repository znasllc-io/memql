package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

func TestCallsSessionKeepsFundingAndHostLeftDeadlinesIndependent(t *testing.T) {
	h := &callsHandler{now: func() time.Time { return callsT0 }}
	session := calls.Session{ExpiresAt: callsT0.Add(time.Hour)}
	hostLeft := callsT0.Add(5 * time.Minute)
	allowance := callsT0.Add(40 * time.Minute).In(time.FixedZone("client offset", -7*60*60))
	reply := h.sessionReply(session, calls.SeatCall{CountdownEndsAt: hostLeft, AllowanceEndsAt: allowance, FundingMode: "usage_preview"})
	require.Equal(t, "usage_preview", reply.FundingMode)
	require.Equal(t, hostLeft.Format(time.RFC3339Nano), reply.CountdownEndsAt)
	require.Equal(t, allowance.UTC().Format(time.RFC3339Nano), reply.AllowanceEndsAt)
	encoded, err := json.Marshal(h.sessionReply(session, calls.SeatCall{}))
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "allowanceEndsAt")
	require.NotContains(t, string(encoded), "fundingMode")
	require.NotContains(t, string(encoded), "countdownEndsAt")
}
