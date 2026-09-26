package edge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestEveryHostedHTMLRouteCarriesItsDeploymentVersion(t *testing.T) {
	for _, kind := range []string{"static", "spa", storefrontKind} {
		t.Run(kind, func(t *testing.T) {
			site := &Site{ID: "site", Hostname: "site.example.test", Kind: kind, Status: "live", BundleRef: "blob://one/"}
			files := mapOpener(map[string]string{
				"index.html":        `<!doctype html><html><head><script>console.log('original')</script></head><body>home</body></html>`,
				"about.html":        `<!doctype html><html><head></head><body>about</body></html>`,
				"nested/index.html": `<h1>nested</h1>`, "legacy.htm": `<h1>legacy</h1>`,
				"app.js": `console.log('asset')`,
			})
			h := NewHandler(Options{Resolver: staticResolver{site: site}, Opener: files})
			paths := []string{"/", "/about", "/nested/", "/legacy.htm"}
			if kind != "static" {
				paths = append(paths, "/client-route")
			}
			for _, path := range paths {
				before := get(t, h, path, nil)
				version := before.Header().Get(deploymentVersionHeader)
				if before.Code != 200 || version == "" || strings.Count(before.Body.String(), `src="`+siteRefreshPath+`"`) != 1 || !strings.Contains(before.Body.String(), `data-memql-version="`+version+`"`) {
					t.Fatalf("%s not monitored: %d %s", path, before.Code, before.Body.String())
				}
				if strings.Contains(directive(before.Header().Get("Content-Security-Policy"), "script-src"), "'unsafe-inline'") {
					t.Fatal("script policy was weakened")
				}
				oldTag := before.Header().Get("ETag")
				site.BundleRef += "new/"
				after := get(t, h, path, map[string]string{"If-None-Match": oldTag})
				if after.Code != 200 || after.Header().Get(deploymentVersionHeader) == version || after.Header().Get("ETag") == oldTag {
					t.Fatal("deploy returned stale page or validator")
				}
				unchanged := get(t, h, path, map[string]string{"If-None-Match": after.Header().Get("ETag")})
				if unchanged.Code != 304 || unchanged.Header().Get(deploymentVersionHeader) != after.Header().Get(deploymentVersionHeader) {
					t.Fatal("unchanged representation did not revalidate")
				}
			}
			asset := get(t, h, "/app.js", nil)
			if asset.Body.String() != `console.log('asset')` {
				t.Fatal("rewrote a non-document")
			}
			script := get(t, h, siteRefreshPath, nil)
			if script.Code != 200 || script.Header().Get("Cache-Control") != "no-store" || script.Body.String() != string(siteRefreshScript) {
				t.Fatal("monitor path fell through to bundle/proxy")
			}
		})
	}
}

func TestVersionCheckAvoidsReadingStoreCredentials(t *testing.T) {
	site := &Site{ID: "site", Kind: storefrontKind, Status: "live", BundleRef: "blob://one/", Store: &BoundStore{ID: "sandbox", Domain: "sandbox.myshopify.com", StorefrontTokenRef: "PUBLIC_TOKEN"}}
	reads := 0
	h := NewHandler(Options{Resolver: staticResolver{site: site}, SecretResolver: func(context.Context, string) (string, error) { reads++; return "public-token", nil }})
	request := httptest.NewRequest(http.MethodHead, runtimeConfigPath, nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.Len() != 0 || reads != 0 || response.Header().Get(deploymentVersionHeader) == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("version check performed config work: reads=%d %v", reads, response.Header())
	}
	before := deploymentVersion(site)
	site.Store = nil
	if before == deploymentVersion(site) {
		t.Fatal("disconnect is invisible to a loaded storefront")
	}
	before = deploymentVersion(site)
	site.Settings = map[string]string{"title": "changed"}
	if before == deploymentVersion(site) {
		t.Fatal("runtime settings update is invisible")
	}
}

func TestSiteRefreshBrowserRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the browser runtime tests (available in CI)")
	}
	cmd := exec.Command(node, "--test", "site_refresh_test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser version monitor: %v\n%s", err, output)
	}
}

func TestVersionPollsDoNotInflateVisitorTraffic(t *testing.T) {
	site := &Site{ID: "site", Kind: "static", Status: "live", BundleRef: "blob://one/"}
	response, records := serveWithLog(t, site, nil, http.MethodHead, runtimeConfigPath)
	if response.Code != http.StatusOK || len(records) != 0 {
		t.Fatalf("background poll recorded as traffic: %d %v", response.Code, records)
	}
}

func TestRefreshScriptOnlyAcceptsReads(t *testing.T) {
	h := NewHandler(Options{Resolver: staticResolver{site: &Site{ID: "site", Kind: "static", Status: "live"}}})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(method, siteRefreshPath, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d", method, response.Code)
		}
	}
}
