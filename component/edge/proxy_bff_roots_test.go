// component/edge/proxy_bff_roots_test.go
package edge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The bff's OWN-ROOT routes reach it through the edge's same-origin marker:
// the Library's byte routes (memql#4343) and the space attachment routes
// (memql#4738).
//
// WHY THIS TEST EXISTS AND WHAT IT PROTECTS. The MemQL Portal is site #1 and
// the MemQL OS is site #2, both served by this handler; the Library's upload
// and export routes and the attachment routes are on the bff at ITS OWN roots
// ("/artifacts", "/spaces"), routed by the front door on api.<domain>. A
// browser at portal.<domain> or os.<domain> POSTing to a bare "/artifacts" or
// "/spaces/{id}/attachments" therefore does not reach the bff at all -- the
// path resolves to no file in the bundle and takes the SPA fallback, so the
// upload is answered with index.html and a 200. That is a silent success for a
// request that stored nothing, which is why a hosted surface addresses these
// routes through the "/_memql" marker instead.
//
// The marker's normal rule swaps itself for the bff's "/memql" route root.
// Applying that here would ask the bff for "/memql/artifacts" or
// "/memql/spaces/...", which it does not serve -- so upstreamPath strips the
// marker outright for these prefixes. Both halves are asserted: that the
// own-root routes lose the marker, and that nothing else does.
func TestAPIProxyStripsTheMarkerForTheBffOwnRootRoutes(t *testing.T) {
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
		// The attachment routes (memql#4738), which are the same fact about a
		// second bff root: the Training app in the MemQL OS drops a file into
		// the caller's own daily space through this.
		{"attachment upload", http.MethodPost, "/_memql/spaces/space-1/attachments", "/spaces/space-1/attachments"},
		{"attachment download", http.MethodGet, "/_memql/spaces/space-1/attachments/att-1", "/spaces/space-1/attachments/att-1"},
		// The bridge, unchanged: the marker is SWAPPED here, not stripped.
		{"the bridge still swaps", http.MethodGet, "/_memql/ws", "/memql/ws"},
		// A path that merely STARTS with the same letters is not one of the
		// bff's roots and must take the ordinary rule -- otherwise a future
		// "/memql/artifactsomething" route would be silently unreachable.
		{"a longer segment is not the prefix", http.MethodGet, "/_memql/artifactsomething", "/memql/artifactsomething"},
		{"a longer segment is not the spaces prefix", http.MethodGet, "/_memql/spacesomething", "/memql/spacesomething"},
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

// The opt-in still governs, for EVERY bff root. A site that did not ask for an
// API path must not reach the Library's byte routes or the attachment routes
// either -- otherwise stripping the marker for them would have turned every
// hosted site into an upload endpoint for the cluster.
//
// Both roots are asserted rather than one standing in for the other: they are
// separate members of `bffRootPrefixes`, and the opt-in check lives upstream of
// the mapping in `serveAPI`, so a refactor that moved it below the strip would
// fail here on whichever root the test happened to name.
func TestBffRootRoutesStillRequireTheAPIOptIn(t *testing.T) {
	h := NewHandler(Options{
		Resolver: staticResolver{site: &Site{
			ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa", APIProxy: false,
		}},
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		APITarget: "http://127.0.0.1:1",
	})

	for _, path := range []string{"/_memql/artifacts", "/_memql/spaces/space-1/attachments"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Host = "shop.example.com"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK {
			t.Errorf("a site with apiProxy=false reached %s", path)
		}
	}
}

// upstreamPath is a pure function and the mapping is the whole decision, so it
// is stated once directly rather than only through the handler.
func TestUpstreamPathMapping(t *testing.T) {
	for in, want := range map[string]string{
		"/_memql/ws":                           "/memql/ws",
		"/_memql/ws/":                          "/memql/ws/",
		"/_memql/artifacts":                    "/artifacts",
		"/_memql/artifacts/":                   "/artifacts/",
		"/_memql/artifacts/a-1/content":        "/artifacts/a-1/content",
		"/_memql/artifactsomething":            "/memql/artifactsomething",
		"/_memql/spaces":                       "/spaces",
		"/_memql/spaces/":                      "/spaces/",
		"/_memql/spaces/s-1/attachments":       "/spaces/s-1/attachments",
		"/_memql/spaces/s-1/attachments/att-1": "/spaces/s-1/attachments/att-1",
		// The negative control for the second prefix, stated here as well as
		// through the handler: a longer FIRST SEGMENT is not the prefix.
		"/_memql/spacesomething": "/memql/spacesomething",
	} {
		if got := upstreamPath(in); got != want {
			t.Errorf("upstreamPath(%q) = %q, want %q", in, got, want)
		}
	}
}
