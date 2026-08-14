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

// serveAPI's own cleanPath guard is the ONLY thing refusing these -- calling
// h.ServeHTTP directly, exactly as every test in this file does, never goes
// through http.ServeMux's redirect. That is precisely the scenario the
// guard's comment names: a Handler exercised (or mounted) some other way
// than behind ServeMux. Each case must never reach the upstream at all --
// reaching it, regardless of what status code comes back, IS the traversal.
func TestAPIProxyRefusesPathsThatDoNotSurviveCleaning(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"dot-segment", "/_memql/../admin"},
		{"doubled slash", "/_memql//double"},
		{"percent-encoded dot-segment", "/_memql/%2e%2e/admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := false
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
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

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Host = "shop.example.com"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if hit {
				t.Fatalf("upstream was reached for %q -- this is the traversal", tc.path)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s = %d, want 400", tc.path, rec.Code)
			}
		})
	}
}

// A trailing slash is not a dot-segment, and cleanPathLikeMux says so:
// http.ServeMux's own cleanPath passes "/_memql/ws/" through unmodified
// (it puts the trailing slash back after path.Clean would otherwise strip
// it), so the guard must too, or a client library that normalizes its own
// URLs with a trailing slash gets a 400 about dot-segments a request like
// this never had. Pins the "match the mux" decision end-to-end: not refused,
// AND the slash survives the hop to the upstream.
func TestAPIProxyPreservesATrailingSlash(t *testing.T) {
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

	req := httptest.NewRequest(http.MethodGet, "/_memql/ws/", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_memql/ws/ = %d, want 200 -- a trailing slash is not a dot-segment", rec.Code)
	}
	if got := rec.Header().Get("X-Upstream-Path"); got != "/memql/ws/" {
		t.Errorf("upstream saw %q, want /memql/ws/ -- the trailing slash must survive the hop", got)
	}
}

// A bare "/_memql" (no trailing slash) never matches apiPrefix at all -- it
// falls through to ordinary static-file resolution instead of ever reaching
// serveAPI's guard. A DIFFERENT reason for the same outcome (the API is
// never reached) than the cases above, and worth pinning separately: a
// change to apiPrefix's matching could silently swap this failure mode for
// an actual proxy call without either the traversal test above or
// TestAPIPrefixIsRefusedWithNoAPITarget catching it. Kind: "static" so the
// fallthrough answers a crisp 404 rather than an spa index.html 200 that
// could be misread as an API response.
func TestBareAPIPrefixWithNoTrailingSlashFallsThroughToStatic(t *testing.T) {
	hit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "static", APIProxy: true,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: upstream.URL,
	})

	req := httptest.NewRequest(http.MethodGet, "/_memql", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if hit {
		t.Error("upstream was reached for a bare /_memql with no trailing slash")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /_memql (static site, no matching file) = %d, want 404", rec.Code)
	}
}
