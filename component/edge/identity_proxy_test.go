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

// memql#4158: identity sets memql_refresh AND the memql_session marker on
// POST /oauth/token. ReverseProxy has a known footgun of collapsing
// multiple Set-Cookie headers; both must reach the browser so a reload
// of / can present the host-only refresh cookie on same-origin
// POST /auth/refresh.
func TestIdentityXHRForwardsBothSetCookieHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     "memql_refresh",
			Value:    "RT-1",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.SetCookie(w, &http.Cookie{
			Name:     "memql_session",
			Value:    "1",
			Path:     "/",
			SameSite: http.SameSiteLaxMode,
		})
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
	req.Host = "portal.memql.localhost"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /oauth/token = %d, want 200 from identity", rec.Code)
	}
	got := rec.Result().Cookies()
	names := map[string]string{}
	for _, c := range got {
		names[c.Name] = c.Value
	}
	if names["memql_refresh"] != "RT-1" {
		t.Errorf("memql_refresh cookie missing or stripped on the proxied response: %#v", got)
	}
	if names["memql_session"] != "1" {
		t.Errorf("memql_session marker missing or stripped on the proxied response: %#v", got)
	}
}

// Reload of / POSTs /auth/refresh same-origin with credentials:include.
// The host-only memql_refresh cookie must survive the hop TO identity;
// stripping it here is "reload returns to authorize".
func TestIdentityXHRForwardsRefreshCookieToIdentity(t *testing.T) {
	var sawCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"AT","token_type":"Bearer","expires_in":900}`))
	}))
	defer upstream.Close()

	h := NewHandler(Options{
		Resolver:       staticResolver{site: spaSite()},
		Opener:         mapOpener(map[string]string{"index.html": "ROOT-SPA"}),
		IdentityTarget: upstream.URL,
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", strings.NewReader(`{}`))
	req.Host = "portal.memql.localhost"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "memql_refresh", Value: "RT-1"})
	req.AddCookie(&http.Cookie{Name: "memql_session", Value: "1"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /auth/refresh = %d, want 200 from identity", rec.Code)
	}
	if !strings.Contains(sawCookie, "memql_refresh=RT-1") {
		t.Errorf("identity did not see memql_refresh; Cookie=%q", sawCookie)
	}
	if !strings.Contains(sawCookie, "memql_session=1") {
		t.Errorf("identity did not see memql_session; Cookie=%q", sawCookie)
	}
}
