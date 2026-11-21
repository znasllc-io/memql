package server

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

func TestSpotlightTransportClosedPayloadAndSharedSessionProjection(t *testing.T) {
	rig := newCallsRig(t)
	spotlight := map[string]any{"roomId": "room", "actorIdentity": "host-generation", "revision": "0", "requestId": "spotlight-request-0001", "seat": "seat", "identity": "target-generation"}
	response := rig.post("/calls/host", callsJSON(t, map[string]any{"action": "spotlight", "spotlight": spotlight}), rig.secrets.hostSession)
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	require.Equal(t, "target-generation", rig.svc.last().host.Spotlight.Identity)
	for _, bad := range []map[string]any{
		{"action": "spotlight"}, {"action": "spotlight", "spotlight": spotlight, "seat": "other"},
		{"action": "lock", "spotlight": spotlight}, {"action": "processing_consent", "spotlight": spotlight},
		{"action": "spotlight", "spotlight": map[string]any{"revision": "0", "forceMediaPermission": true}},
	} {
		before := len(rig.svc.got)
		response = rig.post("/calls/host", callsJSON(t, bad), rig.secrets.hostSession)
		require.Equal(t, http.StatusBadRequest, response.Code)
		for _, call := range rig.svc.got[before:] {
			require.NotEqual(t, "Host", call.op)
		}
	}
	h := &callsHandler{now: rig.clock.now}
	current := calls.SeatCall{Spotlight: &calls.Spotlight{Revision: "revision", Participant: &calls.SpotlightParticipant{Seat: "seat", Identity: "generation", DisplayName: "Name"}}}
	require.Equal(t, current.Spotlight, h.sessionReply(calls.Session{}, current).Spotlight)
	require.Equal(t, current.Spotlight, h.agentReply(current).(callsAgentSessionReply).Spotlight)
	require.Nil(t, h.sessionReply(calls.Session{}, calls.SeatCall{}).Spotlight)
}
