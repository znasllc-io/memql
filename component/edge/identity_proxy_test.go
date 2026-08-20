package edge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func spaSite() *Site {
	return &Site{
		ID: "s1", Hostname: "shop.example.com", Status: "live", Kind: "spa",
	}
}

// memql#4154: POST /oauth/token on a hosted SPA must reach identity, not
// the SPA fallback. Serving index.html as 200 is exactly "identity
// returned no access token (invalid_response)".
func TestIdentityXHRIsProxiedNotSPAFallback(t *testing.T) {
	var sawMethod, sawPath, sawBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod = r.Method
		sawPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		sawBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"AT","token_type":"Bearer","expires_in":900}`))
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver:       staticResolver{site: spaSite()},
		Opener:         mapOpener(map[string]string{"index.html": "ROOT-SPA"}),
		IdentityTarget: upstream.URL,
	})
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(`{"grant_type":"authorization_code"}`))
	req.Host = "shop.example.com"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /oauth/token = %d, want 200 from identity", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"access_token":"AT"`) {
		t.Fatalf("body %q is the SPA fallback or not identity's token JSON", body)
	}
	if sawMethod != http.MethodPost || sawPath != "/oauth/token" {
		t.Errorf("upstream saw %s %s, want POST /oauth/token", sawMethod, sawPath)
	}
	if !strings.Contains(sawBody, "authorization_code") {
		t.Errorf("upstream body %q lost the grant", sawBody)
	}
}

func TestIdentityXHRDoesNotCaptureAuthCallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("identity was reached for %s -- /auth/callback is a site route", r.URL.Path)
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver:       staticResolver{site: spaSite()},
		Opener:         mapOpener(map[string]string{"index.html": "ROOT-SPA"}),
		IdentityTarget: upstream.URL,
	})
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=C&state=S", nil)
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /auth/callback = %d, want 200 SPA fallback", rec.Code)
	}
	if rec.Body.String() != "ROOT-SPA" {
		t.Errorf("GET /auth/callback body = %q, want the SPA", rec.Body.String())
	}
}

func TestIdentityXHRWithoutTargetIsBadGatewayNotHTML(t *testing.T) {
	h := NewHandler(Options{
		Resolver: staticResolver{site: spaSite()},
		Opener:   mapOpener(map[string]string{"index.html": "ROOT-SPA"}),
	})
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(`{}`))
	req.Host = "shop.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST /oauth/token with no identity target = %d, want 502 (not SPA 200)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ROOT-SPA") {
		t.Error("served the SPA fallback as a token response")
	}
}

func TestIsIdentityXHRPathExact(t *testing.T) {
	for _, p := range []string{"/oauth/token", "/auth/refresh", "/auth/logout", "/.well-known/jwks.json"} {
		if !isIdentityXHRPath(p) {
			t.Errorf("%s should be an identity XHR path", p)
		}
	}
	for _, p := range []string{"/auth/callback", "/authorize", "/oauth/token/extra", "/auth/"} {
		if isIdentityXHRPath(p) {
			t.Errorf("%s must not be forwarded to identity", p)
		}
	}
}
