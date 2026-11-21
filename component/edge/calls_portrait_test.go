package edge

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCallsPortraitSelectorDoesNotForwardBearerOrArbitraryQuery(t *testing.T) {
	up := newAccessUpstream(t)
	h := callsHandler(t, callsSite(nil), up.server.URL)
	response := serveAccess(h, accessRequest(http.MethodGet, "/_memql/calls/session?portrait=approved-image", ""))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "portrait=approved-image", up.only(t).rawQuery)
	for _, query := range []string{"portrait=x&token=SECRET", "portrait=x&portrait=y", "portrait=https%3A%2F%2Fevil.test", "portrait="} {
		before := up.count()
		response = serveAccess(h, accessRequest(http.MethodGet, "/_memql/calls/session?"+query, ""))
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.Equal(t, before, up.count())
	}
}
