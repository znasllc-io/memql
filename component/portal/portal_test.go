package portal

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// bundle is a stand-in for a `vite build` output: an index.html, a
// content-hashed asset, and a root-level file that is NOT hashed.
func bundle() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                 {Data: []byte("<!doctype html><div id=root>")},
		"assets/index-abc123.js":     {Data: []byte("console.log(1)")},
		"assets/index-abc123.css":    {Data: []byte(".vk-row{}")},
		"favicon.svg":                {Data: []byte("<svg/>")},
		"nested/dir/placeholder.txt": {Data: []byte("x")},
	}
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	h := New(Options{FS: bundle(), Logger: quiet()})
	if !h.Available() {
		t.Fatal("handler reports no bundle for a fixture that contains index.html")
	}
	return h
}

// get issues a request against the handler with the mount prefix ALREADY
// stripped, which is how it is mounted (app wraps it in http.StripPrefix).
func get(t *testing.T, h *Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

func TestServesHashedAssetImmutably(t *testing.T) {
	resp := get(t, newTestHandler(t), "/assets/index-abc123.js")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want an immutable policy -- the URL carries a "+
			"content hash, so a changed file gets a changed URL and stale bytes are "+
			"unreachable", got)
	}
}

func TestIndexIsNotCached(t *testing.T) {
	resp := get(t, newTestHandler(t), "/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("Cache-Control = %q, want no-cache: index.html's URL is stable while "+
			"its content changes every deploy, and it is what carries the new hashed "+
			"asset URLs", got)
	}
}

// A non-hashed file at the bundle root must NOT inherit the immutable header:
// its URL does not change when its content does.
func TestUnhashedRootAssetIsNotImmutable(t *testing.T) {
	resp := get(t, newTestHandler(t), "/favicon.svg")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want NOT immutable for an unhashed filename", got)
	}
}

// THE SPA FALLBACK. A hard refresh on a client-side route must load the
// application, not 404 -- deep-linkable URLs are a hard requirement of the
// views built on this scaffold.
func TestUnknownPathServesIndexSoClientRoutingSurvivesRefresh(t *testing.T) {
	h := newTestHandler(t)
	for _, path := range []string{
		"/concepts",
		"/concepts/v1:cluster:node",
		"/deep/route/that/only/the/bundle/knows",
	} {
		resp := get(t, h, path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 (index.html fallback)", path, resp.StatusCode)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "id=root") {
			t.Errorf("%s: body = %q, want the index document", path, body)
		}
	}
}

// The fallback must NOT swallow a missing build artefact. A browser holding a
// cached index.html from a previous deploy asks for hashed assets this deploy
// does not have; answering with index.html turns that into "expected a
// JavaScript module script but the server responded with text/html", which
// reads as a MIME problem rather than a missing file.
func TestMissingHashedAssetIs404AndNotTheFallback(t *testing.T) {
	resp := get(t, newTestHandler(t), "/assets/index-deadbeef.js")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "id=root") {
		t.Error("a missing asset was answered with index.html")
	}
}

// A directory is not a file: serving it would emit a listing of the deployed
// bundle. It falls through to the SPA fallback like any other non-file path.
func TestDirectoryDoesNotProduceAListing(t *testing.T) {
	resp := get(t, newTestHandler(t), "/nested/dir")
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "placeholder.txt") {
		t.Fatalf("directory listing leaked: %q", body)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (index.html fallback)", resp.StatusCode)
	}
}

// Path traversal is refused before the filesystem is touched. Go's ServeMux
// already cleans "..", so this is the second of two independent refusals --
// which is the point: this one holds even for a caller that mounts the
// handler without a mux, and for an fs.FS that is not rooted.
func TestRejectsPathTraversal(t *testing.T) {
	h := newTestHandler(t)
	for _, path := range []string{
		"/../etc/passwd",
		"/assets/../../etc/passwd",
		"/./index.html",
		"/assets//index-abc123.js",
	} {
		resp := get(t, h, path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

// A node whose image was built without the portal stage must keep serving
// everything else. It answers 404 with a message that names the cause.
func TestMissingBundleServes404AndDoesNotPanic(t *testing.T) {
	h := New(Options{FS: fstest.MapFS{}, Logger: quiet()})
	if h.Available() {
		t.Fatal("Available() = true for an empty filesystem")
	}
	resp := get(t, h, "/concepts")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), DistDirEnvVar) {
		t.Errorf("body = %q, want it to name %s so an operator can act on it",
			body, DistDirEnvVar)
	}
}

func TestSecurityHeadersAndCSP(t *testing.T) {
	resp := get(t, newTestHandler(t), "/")
	csp := resp.Header.Get("Content-Security-Policy")

	// The two directives the portal cannot work without, and the one it must
	// never relax.
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP %q must pin script-src to 'self' -- the portal ships no inline script", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP %q allows inline script; the portal has none and must not permit any", csp)
	}
	if !strings.Contains(csp, "ws://example.com") {
		t.Errorf("CSP %q must name the same-origin WebSocket origin: the portal's only job "+
			"is to hold /memql/ws open, and Safari has not reliably honoured 'self' for "+
			"ws:", csp)
	}
	if strings.Contains(csp, "upgrade-insecure-requests") {
		t.Errorf("CSP %q emits upgrade-insecure-requests over plain HTTP, which breaks "+
			"a port-forward / preview origin", csp)
	}

	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS emitted over plain HTTP: browsers cache it per host, so one such " +
			"response pins localhost to https forever")
	}
}

func TestTLSRequestGetsHSTSAndUpgradeDirective(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://portal.example.com/", nil)
	req.TLS = &tls.ConnectionState{}
	newTestHandler(t).ServeHTTP(rec, req)

	resp := rec.Result()
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS on a TLS response")
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "upgrade-insecure-requests") {
		t.Errorf("CSP %q lacks upgrade-insecure-requests on TLS", csp)
	}
	if !strings.Contains(csp, "wss://portal.example.com") {
		t.Errorf("CSP %q must name the wss:// origin on a TLS request", csp)
	}
}

// r.Host is whatever the client sent. It must never reach a response header
// unvalidated -- a CR/LF there is header injection, and a space silently
// turns one CSP source into two.
func TestHostileHostIsDroppedFromCSP(t *testing.T) {
	h := newTestHandler(t)
	for _, host := range []string{
		"evil.com' 'unsafe-inline",
		"evil.com\r\nX-Injected: 1",
		"evil.com ws://attacker.example",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		csp := rec.Result().Header.Get("Content-Security-Policy")
		if strings.Contains(csp, "evil.com") {
			t.Errorf("host %q leaked into the CSP: %q", host, csp)
		}
		if !strings.Contains(csp, "connect-src 'self'") {
			t.Errorf("host %q: connect-src must fall back to 'self', got %q", host, csp)
		}
	}
}

func TestDistDirFromEnvDefaultsToImageLayout(t *testing.T) {
	t.Setenv(DistDirEnvVar, "")
	if got := DistDirFromEnv(); got != DefaultDistDir {
		t.Errorf("DistDirFromEnv() = %q, want %q", got, DefaultDistDir)
	}
	t.Setenv(DistDirEnvVar, "  /tmp/portal-dist  ")
	if got := DistDirFromEnv(); got != "/tmp/portal-dist" {
		t.Errorf("DistDirFromEnv() = %q, want the trimmed override", got)
	}
}

// A nil logger must not be a panic waiting to happen at the first boot that
// forgets to pass one.
func TestNilLoggerIsTolerated(t *testing.T) {
	h := New(Options{FS: bundle()})
	if !h.Available() {
		t.Fatal("Available() = false for a populated filesystem")
	}
}
