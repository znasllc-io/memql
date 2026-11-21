package server

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type workflowHandlerFake struct {
	*callsFakeService
	invoked int
}

func (f *workflowHandlerFake) Workflow(context.Context, calls.SiteRef, string, calls.WorkflowRequest) (calls.WorkflowResult, error) {
	f.invoked++
	return calls.WorkflowResult{}, nil
}
func (f *workflowHandlerFake) Resource(context.Context, calls.SiteRef, string, calls.ResourceRequest) (calls.ResourceResult, error) {
	f.invoked++
	return calls.ResourceResult{}, nil
}
func TestWorkflowTransportRequiresOneTypedOperationAndHumanSession(t *testing.T) {
	rig := newCallsRig(t)
	svc := &workflowHandlerFake{callsFakeService: rig.svc}
	h := NewCallsHandler(svc, nil, rig.clock.now)
	for _, tc := range []struct {
		body   string
		cookie bool
		status int
	}{
		{`{"workflow":{"action":"catalog"}}`, false, 401},
		{`{"workflow":{"action":"catalog"}}`, true, 200},
		{`{"resource":{"action":"list"}}`, true, 200},
		{`{"workflow":{"action":"catalog"},"resource":{"action":"list"}}`, true, 400},
		{`{"workflow":{"action":"catalog","url":"https://other.example"}}`, true, 400},
		{`{"resource":{"action":"register","spec":{"label":"Camera","url":"http://169.254.169.254/"}}}`, true, 400},
		{`{"resource":{"action":"start","ssh":"owner@device","command":"whoami"}}`, true, 400},
		{`{"resource":{"action":"start","websocketUrl":"ws://localhost:22","tunnel":true}}`, true, 400},
		{`{}`, true, 400},
	} {
		req := httptest.NewRequest(http.MethodPost, "/calls/workflows", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range rig.stamp {
			req.Header.Set(k, v)
		}
		if tc.cookie {
			req.AddCookie(&http.Cookie{Name: CallsCookieName, Value: rig.secrets.hostSession})
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, tc.status, rec.Code, rec.Body.String())
	}
	require.Equal(t, 2, svc.invoked)
}
