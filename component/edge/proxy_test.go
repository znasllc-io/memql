// component/edge/proxy_test.go
package edge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIProxyForwardsWhenEnabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: true,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: upstream.URL,
	})

	req := httptest.NewRequest(http.MethodGet, "/_memql/ws", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("proxy returned %d", rec.Code)
	}
	if got := rec.Header().Get("X-Upstream-Path"); got != "/memql/ws" {
		t.Errorf("upstream saw %q, want /memql/ws -- the /_memql prefix must be stripped", got)
	}
}

// A site that did not opt in must not get an API path. Otherwise every hosted
// site is an open relay to the cluster's API surface.
func TestAPIProxyIsRefusedWhenNotEnabled(t *testing.T) {
	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: false,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: "http://127.0.0.1:1",
	})

	req := httptest.NewRequest(http.MethodGet, "/_memql/ws", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Error("a site with apiProxy=false reached the API")
	}
}

// The WebSocket upgrade must survive the hop. This is the failure mode a unit
// test on a plain GET would miss entirely.
func TestAPIProxyPreservesTheUpgradeHeaders(t *testing.T) {
	var sawUpgrade, sawConnection string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUpgrade = r.Header.Get("Upgrade")
		sawConnection = r.Header.Get("Connection")
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: true,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: upstream.URL,
	})

	req := httptest.NewRequest(http.MethodGet, "/_memql/ws", nil)
	req.Host = "shop.example.com"
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !strings.EqualFold(sawUpgrade, "websocket") {
		t.Errorf("upstream saw Upgrade=%q; the proxy dropped it", sawUpgrade)
	}
	if !strings.Contains(strings.ToLower(sawConnection), "upgrade") {
		t.Errorf("upstream saw Connection=%q; the proxy dropped it", sawConnection)
	}
}
