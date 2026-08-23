// component/edge/proxy_library_test.go
package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Library's byte routes reach the bff through the edge's same-origin
// marker (memql#4343).
//
// WHY THIS TEST EXISTS AND WHAT IT PROTECTS. The MemQL Portal is site #1,
// served by this handler at portal.<domain>; the Library's upload and export
// routes are on the bff at ITS OWN root ("/artifacts"), routed by the front
// door on api.<domain>. A browser at portal.<domain> POSTing to a bare
// "/artifacts" therefore does not reach the bff at all -- the path resolves to
// no file in the bundle and takes the SPA fallback, so the upload is answered
// with index.html and a 200. That is a silent success for a request that
// stored nothing, which is why the portal addresses these routes through the
// "/_memql" marker instead.
//
// The marker's normal rule swaps itself for the bff's "/memql" route root.
// Applying that here would ask the bff for "/memql/artifacts", which it does
// not serve -- so upstreamPath strips the marker outright for this one prefix.
// Both halves are asserted: that the byte routes lose the marker, and that
// nothing else does.
func TestAPIProxyStripsTheMarkerForTheLibraryByteRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Path", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "portal", Hostname: "portal.example.com", Status: "live", Kind: "spa", APIProxy: true,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: upstream.URL,
	})

	cases := []struct {
		name   string
		method string
		in     string
		want   string
	}{
		{"upload", http.MethodPost, "/_memql/artifacts", "/artifacts"},
		{"export", http.MethodGet, "/_memql/artifacts/artifact-1/content", "/artifacts/artifact-1/content"},
		// The trailing-slash spelling server.ArtifactPaths() also emits.
		{"collection with a trailing slash", http.MethodPost, "/_memql/artifacts/", "/artifacts/"},
		// The bridge, unchanged: the marker is SWAPPED here, not stripped.
		{"the bridge still swaps", http.MethodGet, "/_memql/ws", "/memql/ws"},
		// A path that merely STARTS with the same letters is not the Library's
		// prefix and must take the ordinary rule -- otherwise a future
		// "/memql/artifactsomething" route would be silently unreachable.
		{"a longer segment is not the prefix", http.MethodGet, "/_memql/artifactsomething", "/memql/artifactsomething"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.in, nil)
			req.Host = "portal.example.com"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200 -- the request never reached the API target",
					tc.method, tc.in, rec.Code)
			}
			if got := rec.Header().Get("X-Upstream-Path"); got != tc.want {
				t.Errorf("%s %s reached the bff as %q, want %q", tc.method, tc.in, got, tc.want)
			}
		})
	}
}

// The opt-in still governs. A site that did not ask for an API path must not
// reach the Library's byte routes either -- otherwise this change would have
// turned every hosted site into an upload endpoint for the cluster's Library.
func TestLibraryByteRoutesStillRequireTheAPIOptIn(t *testing.T) {
	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: false,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: "http://127.0.0.1:1",
	})

	req := httptest.NewRequest(http.MethodPost, "/_memql/artifacts", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Error("a site with apiProxy=false reached the Library's upload route")
	}
}

// upstreamPath is a pure function and the mapping is the whole decision, so it
// is stated once directly rather than only through the handler.
func TestUpstreamPathMapping(t *testing.T) {
	for in, want := range map[string]string{
		"/_memql/ws":                     "/memql/ws",
		"/_memql/ws/":                    "/memql/ws/",
		"/_memql/artifacts":              "/artifacts",
		"/_memql/artifacts/":             "/artifacts/",
		"/_memql/artifacts/a-1/content":  "/artifacts/a-1/content",
		"/_memql/artifactsomething":      "/memql/artifactsomething",
		"/_memql/spaces/s-1/attachments": "/memql/spaces/s-1/attachments",
	} {
		if got := upstreamPath(in); got != want {
			t.Errorf("upstreamPath(%q) = %q, want %q", in, got, want)
		}
	}
}
