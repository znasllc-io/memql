package server

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestProcessingConsentTransportBindsAdmissionAndRejectsMixedAuthority(t *testing.T) {
	rig := newCallsRig(t)
	body := map[string]any{"action": "processing_consent", "processing": map[string]any{"roomId": "room", "identity": "current-admission", "revision": "0", "safety": true, "transcription": false, "insights": false}}
	response := rig.post("/calls/host", callsJSON(t, body), rig.secrets.hostSession)
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	got := rig.svc.got[len(rig.svc.got)-1].host
	require.NotNil(t, got.Processing)
	require.Equal(t, "current-admission", got.Processing.Identity)
	for _, bad := range []map[string]any{{"action": "processing_consent"}, {"action": "processing_consent", "processing": body["processing"], "seat": "someone-else"}, {"action": "lock", "processing": body["processing"]}, {"action": "processing_consent", "processing": map[string]any{"roomId": "room", "identity": "identity", "revision": "0", "otherSeat": "not-mine"}}} {
		before := len(rig.svc.got)
		response = rig.post("/calls/host", callsJSON(t, bad), rig.secrets.hostSession)
		require.Equal(t, http.StatusBadRequest, response.Code)
		for _, call := range rig.svc.got[before:] {
			require.NotEqual(t, "Host", call.op)
		}
	}
}
