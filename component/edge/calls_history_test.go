package edge

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallsHistoryForwardsOnlyItsBoundedQueryAllowlist(t *testing.T) {
	up := newAccessUpstream(t)
	h := callsHandler(t, callsSite(nil), up.server.URL)
	raw := "archive=1&limit=50&period=7&status=ended&search=team%20review"
	response := serveAccess(h, accessRequest(http.MethodGet, "/_memql/calls/history?"+raw, ""))
	require.Equal(t, http.StatusOK, response.Code)
	hit := up.only(t)
	require.Equal(t, "/calls/history", hit.path)
	require.Equal(t, raw, hit.rawQuery)
	requireCallsStamp(t, hit)
}
func TestCallsHistoryRejectsUnknownDuplicateAndOversizedQueriesBeforeProxy(t *testing.T) {
	for _, query := range []string{"key=secret", "accountId=other", "limit=51", "limit=1&limit=2", "token=secret", "search=%zz"} {
		t.Run(query, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)
			response := serveAccess(h, accessRequest(http.MethodGet, "/_memql/calls/history?"+query, ""))
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Zero(t, up.count())
		})
	}
	up := newAccessUpstream(t)
	h := callsHandler(t, callsSite(nil), up.server.URL)
	response := serveAccess(h, accessRequest(http.MethodPost, "/_memql/calls/history?archive=1", `{"action":"archive"}`))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Zero(t, up.count())
}
