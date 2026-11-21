package edge

import (
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReportReadsDoNotConsumeAuthenticationBudget(t *testing.T) {
	for _, body := range []string{`{"action":"report_list","report":{}}`, `{"report":{"cursor":"next"},"action":"report_list"}`, `{"action":"report_status","report":{"id":"` + strings.Repeat("a", 64) + `"}}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		require.True(t, callsAccountIsReportRead(r))
		after, e := io.ReadAll(r.Body)
		require.NoError(t, e)
		require.Equal(t, body, string(after))
	}
	for _, body := range []string{`{"action":"report_list"}`, `{"action":"report_list","report":null}`, `{"action":"report_list","report":{},"action":"login_begin"}`, `{"Action":"login_begin","action":"report_list","report":{}}`, `{"action":"report_list","report":{"cursor":"a","cursor":"b"}}`, `{"action":"report_submit","report":{}}`, `{"action":"report_list","report":{},"key":"secret"}`, `{"action":"report_list","report":{"description":"hidden"}}`, `{"action":"report_list","report":{}} {}`, `{"action":"report_status","report":{}}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		require.False(t, callsAccountIsReportRead(r), body)
	}
}
func TestReportPaginationUsesReadBudgetWithoutBypassingLoginLimit(t *testing.T) {
	up := newAccessUpstream(t)
	h := callsHandler(t, callsSite(nil), up.server.URL)
	address := "203.0.113.88"
	for i := 0; i < 11; i++ {
		rec := serveAccess(h, accountRateRequest(`{"action":"report_list","report":{}}`, address))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	for i := 0; i < 10; i++ {
		rec := serveAccess(h, accountRateRequest(`{"action":"login_begin"}`, address))
		require.Equal(t, http.StatusOK, rec.Code)
	}
	requireCallsLimited(t, serveAccess(h, accountRateRequest(`{"action":"login_begin"}`, address)), 6000, "6")
	rec := serveAccess(h, accountRateRequest(`{"action":"report_list","report":{}}`, address))
	require.Equal(t, http.StatusOK, rec.Code)
	requireCallsLimited(t, serveAccess(h, accountRateRequest(`{"action":"report_list","report":{},"action":"login_begin"}`, address)), 6000, "6")
}
