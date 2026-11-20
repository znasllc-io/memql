package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

func (f *callsFakeService) LeaveWithOptions(_ context.Context, sess calls.Session, req calls.LeaveRequest) error {
	f.record(callsFakeCall{op: "LeaveWithOptions", session: &sess, leave: req})
	if r, ok := f.refusal("LeaveWithOptions"); ok {
		return r.err
	}
	_, err := f.known(sess)
	return err
}
func TestCallsBoundLeaveRoutesPreserveExactRequestAndCookie(t *testing.T) {
	for _, mode := range []string{"", "keep_running", "wrap_up"} {
		t.Run(mode, func(t *testing.T) {
			rig := newCallsRig(t)
			req := calls.LeaveRequest{Mode: mode, LeaveToken: "opaque.signed", IdempotencyKey: strings.Repeat("a", 24)}
			data, err := json.Marshal(req)
			require.NoError(t, err)
			rec := rig.post("/calls/leave", string(data), rig.secrets.hostSession)
			assertCallsReply(t, rec, http.StatusNoContent, "")
			assertNoCallsCookie(t, rec)
			var got *calls.LeaveRequest
			for _, call := range rig.svc.got {
				if call.op == "LeaveWithOptions" {
					v := call.leave
					got = &v
				}
			}
			require.NotNil(t, got)
			require.Equal(t, req, *got)
		})
	}
	rig := newCallsRig(t)
	rig.svc.refusals["LeaveWithOptions"] = callsFakeRefusal{err: calls.Refuse(calls.ErrConflict, "leave_changed", "stale admission")}
	req := calls.LeaveRequest{Mode: "wrap_up", LeaveToken: "opaque", IdempotencyKey: strings.Repeat("a", 24)}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	rec := rig.post("/calls/leave", string(body), rig.secrets.hostSession)
	assertCallsReply(t, rec, http.StatusConflict, `{"error":"conflict","reason":"leave_changed"}`)
	assertNoCallsCookie(t, rec)
}
func TestCallsSessionExposesFirmCountdownAndBoundIdentity(t *testing.T) {
	h := &callsHandler{now: func() time.Time { return callsT0 }}
	got := h.sessionReply(calls.Session{ExpiresAt: callsT0.Add(time.Hour)}, calls.SeatCall{Identity: "media-identity", LeaveToken: "opaque.signed", CountdownKind: "wrap_up", CountdownEndsAt: callsT0.Add(5 * time.Minute)})
	require.Equal(t, "media-identity", got.Identity)
	require.Equal(t, "opaque.signed", got.LeaveToken)
	require.Equal(t, "wrap_up", got.CountdownKind)
	empty, err := json.Marshal(h.sessionReply(calls.Session{ExpiresAt: callsT0.Add(time.Hour)}, calls.SeatCall{}))
	require.NoError(t, err)
	for _, field := range []string{"identity", "leaveToken", "countdownKind"} {
		require.NotContains(t, string(empty), field)
	}
}
