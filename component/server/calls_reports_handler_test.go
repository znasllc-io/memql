package server

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
	"net/http"
	"testing"
)

func TestReportTransportClosedMetadataAndAgentSessionCapabilities(t *testing.T) {
	rig, svc := accountHandlerRig(t)
	body := `{"action":"report_submit","report":{"roomId":"room","identity":"admission","category":"other","description":"Benign observation","idempotencyKey":"report-request-0001"}}`
	rec := rig.send(http.MethodPost, "/calls/account", body, rig.secrets.guestSession)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "admission", svc.request.Report.Identity)
	for _, field := range []string{"screenshot", "recordingUrl", "reporterName", "subjectAge", "actorId"} {
		rig, svc = accountHandlerRig(t)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &parsed))
		parsed["report"].(map[string]any)[field] = "untrusted"
		rec = rig.send(http.MethodPost, "/calls/account", callsJSON(t, parsed), rig.secrets.guestSession)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Zero(t, svc.calls)
	}
	h := &callsHandler{now: rig.clock.now}
	current := calls.SeatCall{Reports: calls.ReportCapabilities{Available: true, MetadataDays: 1}, Processing: &calls.ProcessingStatus{Available: true, Consent: calls.ProcessingConsent{Revision: "current-consent"}}}
	reply := h.agentReply(current).(callsAgentSessionReply)
	require.Equal(t, current.Reports, reply.Reports)
	require.Equal(t, current.Processing, reply.Processing)
}
