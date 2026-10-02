package edge

import (
	"encoding/json"
	identityweb "github.com/znasllc-io/memql/component/identity/web"
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
	if body := sourceHTML(rec.Body.String()); !strings.Contains(body, `"access_token":"AT"`) {
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
	if sourceHTML(rec.Body.String()) != "ROOT-SPA" {
		t.Errorf("GET /auth/callback body = %q, want the SPA", sourceHTML(rec.Body.String()))
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
	if strings.Contains(sourceHTML(rec.Body.String()), "ROOT-SPA") {
		t.Error("served the SPA fallback as a token response")
	}
}

func TestIsIdentityXHRPathExact(t *testing.T) {
	for _, p := range []string{"/oauth/token", "/auth/refresh", "/auth/logout", "/.well-known/jwks.json", "/auth/github/complete"} {
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

func TestGitHubCompletionForwardsTheOSCookieAndCallbackToIdentity(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		cookie, err := r.Cookie("memql_refresh")
		if err != nil || cookie.Value != "os-host-session" || r.URL.Path != "/auth/github/complete" || r.URL.Query().Get("code") != "provider-code" || r.URL.Query().Get("state") != "single-use-state" || r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Error("completion lost the OS browser session or callback data at the proxy hop")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, "https://os.example.com/?connect=settings&github=connected", http.StatusSeeOther)
	}))
	defer upstream.Close()
	h := NewHandler(Options{Resolver: staticResolver{site: spaSite()}, Opener: mapOpener(map[string]string{"index.html": "ROOT-SPA"}), IdentityTarget: upstream.URL})
	req := httptest.NewRequest(http.MethodGet, "https://shop.example.com/auth/github/complete?code=provider-code&state=single-use-state", nil)
	req.AddCookie(&http.Cookie{Name: "memql_refresh", Value: "os-host-session"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "connect=settings") || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("GitHub completion must proxy to Identity and return its redirect, never the SPA fallback")
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

func TestShopifyCompletionProxiesSessionAndSignedQuery(t *testing.T) {
	raw := "state=s&hmac=h&extra=a%2Bb&extra=x%20y"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("memql_refresh")
		if err != nil || cookie.Value != "session" || r.URL.RawQuery != raw || r.URL.Path != "/auth/shopify/complete" {
			t.Error("Shopify completion lost its session or signed parameters")
		}
		w.Header().Set("Location", "https://os.example.test/?shopify=connected&site=s1")
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer upstream.Close()
	h := NewHandler(Options{Resolver: staticResolver{site: spaSite()}, Opener: mapOpener(map[string]string{"index.html": "SPA"}), IdentityTarget: upstream.URL})
	req := httptest.NewRequest("GET", "/auth/shopify/complete?"+raw, nil)
	req.Host = "shop.example.com"
	req.AddCookie(&http.Cookie{Name: "memql_refresh", Value: "session"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("completion returned %d, want identity redirect", rec.Code)
	}
}

// A browser that refuses third-party cookies must never need one here: the
// native GET and POST stay on the OS host. Exercise the real proxy hop and
// identity's CSRF middleware, including the rejection cases.
func TestNativeIdentityKeepsCookiePairThroughProxy(t *testing.T) {
	var writes int
	secure := false
	check := identityweb.CSRFMiddleware(identityweb.CSRFOptions{Secure: &secure})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"csrf": identityweb.CSRFTokenFromRequest(r)})
	}))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "https://os.example.com" {
			http.Error(w, "origin refused", 403)
			return
		}
		check.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	h := NewHandler(Options{Resolver: staticResolver{site: spaSite()}, Opener: mapOpener(map[string]string{"index.html": "SPA"}), IdentityTarget: upstream.URL})
	read := httptest.NewRequest("GET", "https://os.example.com/device?user_code=ABCD-2345", nil)
	read.Header.Set("Accept", identityweb.NativeMediaType)
	page := httptest.NewRecorder()
	h.ServeHTTP(page, read)
	if page.Code != 200 {
		t.Fatal(page.Code, page.Body.String())
	}
	cookies := page.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Domain != "" || !cookies[0].HttpOnly {
		t.Fatal("expected a host-only HttpOnly CSRF cookie")
	}
	var body map[string]string
	if err := json.Unmarshal(page.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, origin, token string
		cookie              bool
		status              int
	}{
		{"approved", "https://os.example.com", body["csrf"], true, 200},
		{"missing cookie", "https://os.example.com", body["csrf"], false, 403},
		{"stale page", "https://os.example.com", "wrong", true, 403},
		{"foreign origin", "https://evil.test", body["csrf"], true, 403},
		{"missing origin", "", body["csrf"], true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "https://os.example.com/device", strings.NewReader("action=approve&user_code=ABCD-2345"))
			req.Header.Set("Accept", identityweb.NativeMediaType)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-CSRF-Token", tc.token)
			if tc.cookie {
				req.AddCookie(cookies[0])
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatal(rec.Code, rec.Body.String())
			}
		})
	}
	if writes != 1 {
		t.Fatalf("%d writes, want exactly one authorized action", writes)
	}
}

func TestNativeIdentityRoutingIsExactAndRepresentationScoped(t *testing.T) {
	for _, tc := range []struct {
		path, accept string
		want         bool
	}{
		{"/device", identityweb.NativeMediaType, true}, {"/auth/setup/state", identityweb.NativeMediaType, true},
		{"/device", "text/html", false}, {"/identity/device", identityweb.NativeMediaType, false},
		{"/auth/callback", identityweb.NativeMediaType, false}, {"/device/extra", identityweb.NativeMediaType, false},
		{"/admin/", identityweb.NativeMediaType, false},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Accept", tc.accept)
		if got := isIdentityUIRequest(req); got != tc.want {
			t.Errorf("%s %s: %v", tc.path, tc.accept, got)
		}
	}
	req := httptest.NewRequest("POST", "/auth/webauthn/login/finish", nil)
	if !isIdentityPasskeyRequest(req) {
		t.Fatal("passkey finish must retain the first-party cookie")
	}
	req.Method = "GET"
	if isIdentityPasskeyRequest(req) {
		t.Fatal("must not capture a page navigation")
	}
}
