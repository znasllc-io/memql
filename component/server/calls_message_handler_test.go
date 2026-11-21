package server

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

type relayHandlerService struct {
	*callsFakeService
	request calls.MessageRequest
	result  calls.MessageResult
	count   int
}

func (s *relayHandlerService) Message(_ context.Context, _ calls.Session, q calls.MessageRequest) (calls.MessageResult, error) {
	s.count++
	s.request = q
	return s.result, nil
}
func TestMessageTransportClosedShapeStatusAndPrivateLogs(t *testing.T) {
	rig := newCallsRig(t)
	svc := &relayHandlerService{callsFakeService: rig.svc}
	rig.h = NewCallsHandler(svc, slog.New(slog.NewTextHandler(rig.logs, nil)), rig.clock.now)
	body := `{"roomId":"room1","identity":"generation","sequence":1,"kind":"chat","text":"private benign relay text"}`
	for _, tc := range []struct {
		status string
		http   int
	}{{"published", 200}, {"duplicate", 200}, {"stale", 409}, {"rate_limited", 429}, {"unavailable", 503}, {"delivery_uncertain", 503}} {
		svc.result = calls.MessageResult{Status: tc.status, Sequence: 1, NextSequence: 2, RetryAfterMS: 400}
		res := rig.post("/calls/messages", body, rig.secrets.hostSession)
		require.Equal(t, tc.http, res.Code, res.Body.String())
		require.Equal(t, "private benign relay text", *svc.request.Text)
		require.NotContains(t, res.Body.String(), "private benign relay text")
		if tc.status == "rate_limited" {
			require.Equal(t, "1", res.Header().Get("Retry-After"))
		}
	}
	before := svc.count
	for _, body := range []string{strings.Replace(body, `"text":`, `"forgedSender":"other","text":`, 1), strings.Repeat("x", 13<<10)} {
		res := rig.post("/calls/messages", body, rig.secrets.hostSession)
		require.Contains(t, []int{400, 413}, res.Code)
		require.Equal(t, before, svc.count)
	}
	res := rig.post("/calls/messages", body, "")
	require.Equal(t, http.StatusUnauthorized, res.Code)
	require.Equal(t, before, svc.count)
	require.NotContains(t, rig.logs.String(), "private benign relay text")
	require.NotContains(t, rig.logs.String(), rig.secrets.hostSession)
}
