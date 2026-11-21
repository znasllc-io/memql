package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentReviewHostTransportBindsExactDigestAndRejectsMixedAuthority(t *testing.T) {
	rig := newCallsRig(t)
	valid := map[string]string{"action": "agent_approve", "requestId": strings.Repeat("a", 64), "reviewDigest": strings.Repeat("b", 64)}
	response := rig.post("/calls/host", callsJSON(t, valid), rig.secrets.hostSession)
	require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
	got := rig.svc.got[len(rig.svc.got)-1].host
	require.Equal(t, valid["requestId"], got.RequestID)
	require.Equal(t, valid["reviewDigest"], got.ReviewDigest)
	for _, mixed := range []map[string]string{
		{"action": "agent_approve", "requestId": valid["requestId"]},
		{"action": "agent_approve", "requestId": valid["requestId"], "reviewDigest": valid["reviewDigest"], "seat": "other-seat"},
		{"action": "lock", "requestId": valid["requestId"], "reviewDigest": valid["reviewDigest"]},
	} {
		before := len(rig.svc.got)
		response = rig.post("/calls/host", callsJSON(t, mixed), rig.secrets.hostSession)
		require.Equal(t, http.StatusBadRequest, response.Code)
		for _, call := range rig.svc.got[before:] {
			require.NotEqual(t, "Host", call.op)
		}
	}
}
