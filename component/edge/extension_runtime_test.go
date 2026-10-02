package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func extensionSite() *Site {
	return &Site{ID: "editor", Hostname: "editor.example.test", Kind: "static", Status: "live", ExtensionRuntimePath: "/editor/", APIProxy: true}
}

const resourceHost = "editor--assets--v--0123456789abcdefghijklmnop.example.test"

func TestExtensionRuntimePolicyIsScoped(t *testing.T) {
	site := extensionSite()
	files := map[string]string{"index.html": "<h1>Landing</h1>", "editor/index.html": "<h1>Editor</h1>", "editor/assets/extension-host.html": "<script>worker()</script>"}
	plain := serve(t, site, files, "/").Header().Get("Content-Security-Policy")
	if strings.Contains(plain, "worker-src") || strings.Contains(plain, "unsafe-eval") {
		t.Fatal(plain)
	}
	app := serve(t, site, files, "/editor/").Header().Get("Content-Security-Policy")
	if !strings.Contains(app, "worker-src 'self' blob:") || !strings.Contains(app, "'wasm-unsafe-eval'") || strings.Contains(app, "'unsafe-eval'") || !strings.Contains(app, "frame-ancestors 'none'") {
		t.Fatal(app)
	}
	host := serve(t, site, files, "/editor/assets/extension-host.html")
	policy := host.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "'unsafe-eval'") || !strings.Contains(policy, "frame-ancestors 'self'") || host.Header().Get("X-Frame-Options") != "" {
		t.Fatal(policy, host.Header())
	}
	site.ExtensionRuntimePath = ""
	disabled := serve(t, site, files, "/editor/assets/extension-host.html")
	if strings.Contains(disabled.Header().Get("Content-Security-Policy"), "'unsafe-eval'") {
		t.Fatal("runtime permission survived removal")
	}
}

func TestIsolatedExtensionOriginsServeOnlyAssets(t *testing.T) {
	site := extensionSite()
	// Even a parent with SPA fallback must never expose its app document on
	// the renderer origin when an extension asset is missing.
	site.Kind = "spa"
	site.ResourceParentHost, site.Hostname = site.Hostname, resourceHost
	files := mapOpener{"index.html": "private app", "editor/assets/fake.html": "<html></html>", "editor/extensions/core.js": "extension code"}
	handler := NewHandler(Options{Resolver: staticResolver{site}, Opener: files})
	for _, target := range []string{"/", "/editor/", "/_memql/runtime-config", "/_memql/ws", "/_memql/preview/enter", "/editor/extensions/core.js", "/editor/assets/../../index.html", "/editor/assets/missing", "/editor/assets/missing/"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/editor/assets/fake.html", nil))
	if response.Code != http.StatusOK || response.Header().Get("X-Frame-Options") != "" || response.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal(response.Code, response.Header())
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors https://editor.example.test 'self'") {
		t.Fatal(response.Header())
	}
	site.Status = "draft"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/editor/assets/fake.html", nil))
	if response.Code != http.StatusNotFound {
		t.Fatal("draft assets exposed", response.Code)
	}
}

func TestExtensionResourceOriginsCannotReadAppOrAPIs(t *testing.T) {
	site := extensionSite()
	for _, origin := range []string{"https://" + resourceHost, "https://evil.test", "https://" + resourceHost + ":444", "https://user@" + resourceHost} {
		for _, target := range []string{"/editor/", "/_memql/runtime-config", "/editor/assets/main.js"} {
			request := httptest.NewRequest(http.MethodGet, target, nil)
			request.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			extensionRuntimePolicy(response, request, site, "default-src 'self'")
			allowed := response.Header().Get("Access-Control-Allow-Origin") != ""
			if allowed != (origin == "https://"+resourceHost && target == "/editor/assets/main.js") {
				t.Fatalf("%s %s: %v", origin, target, response.Header())
			}
		}
	}
	if got := extensionResourceParent(resourceHost); got != "editor.example.test" {
		t.Fatal(got)
	}
	for _, host := range []string{"editor.example.test", "editor--assets--short.example.test", "editor--assets--01234567890123456789012345678901234567890123456789012345678901234.example.test"} {
		if extensionResourceParent(host) != "" {
			t.Fatal(host)
		}
	}
}

func TestIsolatedOriginFollowsCanonicalInvalidationOnEveryReplica(t *testing.T) {
	site := extensionSite()
	ex := &stubExec{rows: map[string]*Site{site.Hostname: site}}
	first, second := NewResolver(ex, time.Minute), NewResolver(ex, time.Minute)
	for _, replica := range []Resolver{first, second} {
		got, err := replica.Resolve(context.Background(), resourceHost)
		if err != nil || got == nil || got.APIProxy || got.ResourceParentHost != site.Hostname {
			t.Fatalf("%+v %v", got, err)
		}
	}
	replacement := *site
	replacement.ExtensionRuntimePath = ""
	ex.rows[site.Hostname] = &replacement
	for _, replica := range []Resolver{first, second} {
		replica.Invalidate(site.Hostname)
		got, err := replica.Resolve(context.Background(), resourceHost)
		if err != nil || got != nil {
			t.Fatalf("stale isolation permission %+v %v", got, err)
		}
	}
}

func TestBrowserRuntimeOwnsUnsavedEditorLifecycle(t *testing.T) {
	site := extensionSite()
	files := map[string]string{"editor/index.html": "<h1>Editor</h1>", "editor/assets/extension-host.html": "<script>worker()</script>"}
	for _, target := range []string{"/editor/", "/editor/assets/extension-host.html"} {
		response := serve(t, site, files, target)
		if response.Code != 200 || strings.Contains(response.Body.String(), siteRefreshPath) {
			t.Fatal(target, response.Code, response.Body.String())
		}
	}
}
