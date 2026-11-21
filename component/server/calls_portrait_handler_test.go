package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/calls"
)

type portraitHandlerFake struct {
	*callsFakeService
	image calls.PortraitImage
	seen  string
}

func (f *portraitHandlerFake) Portrait(_ context.Context, _ calls.Session, id string) (calls.PortraitImage, error) {
	f.seen = id
	return f.image, nil
}
func TestPortraitTransportRequiresCookieAndReturnsOnlyIntegrityCheckedRaster(t *testing.T) {
	rig := newCallsRig(t)
	data := []byte("canonical-test-image")
	svc := &portraitHandlerFake{callsFakeService: rig.svc, image: calls.PortraitImage{Portrait: calls.PortraitInfo{ID: "asset", Digest: calls.PortraitContentDigest(data), MimeType: "image/png"}, Data: data}}
	h := NewCallsHandler(svc, nil, rig.clock.now)
	get := func(cookie, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/calls/session?"+query, nil)
		for k, v := range rig.stamp {
			r.Header.Set(k, v)
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: CallsCookieName, Value: cookie})
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, r)
		return response
	}
	response := get("", "portrait=asset")
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Empty(t, svc.seen)
	response = get(rig.secrets.hostSession, "portrait=asset&token=secret")
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Empty(t, svc.seen)
	response = get(rig.secrets.hostSession, "portrait=asset")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "asset", svc.seen)
	require.Equal(t, data, response.Body.Bytes())
	require.Equal(t, "image/png", response.Header().Get("Content-Type"))
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", response.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "same-origin", response.Header().Get("Cross-Origin-Resource-Policy"))
	svc.image.Data = []byte("different bytes")
	response = get(rig.secrets.hostSession, "portrait=asset")
	require.NotEqual(t, http.StatusOK, response.Code)
	require.NotContains(t, response.Body.String(), "different bytes")
}
