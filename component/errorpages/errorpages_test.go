package errorpages

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// errorpages_test.go -- the maintenance pages ingress-nginx falls back to.
//
// On 2026-10-11 the whole mesh was down and every host answered ingress-nginx's
// bare "503 Service Temporarily Unavailable". These tests hold the replacement
// to its three promises: the platform's own hosts get the MemQL page, every
// other host (a customer's site, a custom domain) gets a page that never says
// whose platform it runs on, and an API client gets JSON it can parse.

const testDomain = "memql.example"

const (
	platformHost = "os.memql.example"
	siteHost     = "www.customer.com"

	platformMessage = "MemQL is being updated. It will be back in a few minutes."
	siteMessage     = "This site is temporarily unavailable. It will be back soon."

	// maxPageBytes is the budget for one page: a page that has to work while
	// everything else is down should not be the heavy thing.
	maxPageBytes = 20 * 1024
)

// serve issues one request against a handler built for domain and records the
// answer. host overrides the Host header the way ingress-nginx would deliver
// it, which httptest.NewRequest otherwise pins to example.com.
func serve(t *testing.T, domain, method, host, target string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	Handler(domain).ServeHTTP(rec, req)
	return rec
}

func page(t *testing.T, host string) string {
	t.Helper()
	rec := serve(t, testDomain, http.MethodGet, host, "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET / on %s answered %d, want 503", host, rec.Code)
	}
	return rec.Body.String()
}

func TestClassifyHost(t *testing.T) {
	cases := []struct {
		name   string
		host   string
		domain string
		want   Kind
	}{
		// The platform's own hosts: the apex and the five labels under it.
		{"the apex", "memql.example", testDomain, KindPlatform},
		{"the apex in upper case with a port", "OS.Memql.Example:443", testDomain, KindPlatform},
		{"api", "api.memql.example", testDomain, KindPlatform},
		{"identity", "identity.memql.example", testDomain, KindPlatform},
		{"mcp", "mcp.memql.example", testDomain, KindPlatform},
		{"os", "os.memql.example", testDomain, KindPlatform},
		{"vscode", "vscode.memql.example", testDomain, KindPlatform},
		{"a fully qualified apex", "memql.example.", testDomain, KindPlatform},
		{"a fully qualified host with a port", "api.memql.example.:8443", testDomain, KindPlatform},
		{"a padded host", "  os.memql.example  ", testDomain, KindPlatform},
		{"a domain value in upper case", "os.memql.example", "MemQL.Example", KindPlatform},
		{"a domain value with padding and a trailing dot", "os.memql.example", " memql.example. ", KindPlatform},

		// Everything else is somebody's site, and a site must never be branded.
		{"a site under the domain", "shop.memql.example", testDomain, KindSite},
		{"another domain", "customer.com", testDomain, KindSite},
		{"www on another domain", "www.customer.com", testDomain, KindSite},
		{"a look-alike prefix", "evil-memql.example", testDomain, KindSite},
		{"a platform host as the prefix of another domain", "os.memql.example.evil.test", testDomain, KindSite},
		{"a host nested under a platform host", "a.os.memql.example", testDomain, KindSite},
		{"the wildcard spelled out", "*.memql.example", testDomain, KindSite},
		{"an IPv4 address", "127.0.0.1:8080", testDomain, KindSite},
		{"an IPv6 address", "[::1]:8080", testDomain, KindSite},
		{"no host at all", "", testDomain, KindSite},

		// No domain configured: nothing can be called the platform's, so every
		// host gets the neutral page. The unconfigured case fails toward not
		// branding somebody's site.
		{"an empty domain, a platform-shaped host", "os.memql.example", "", KindSite},
		{"an empty domain, the apex", "memql.example", "", KindSite},
		{"an empty domain, an api host", "api.memql.example", "", KindSite},
		{"an empty domain, no host", "", "", KindSite},
		{"a blank domain", "os.memql.example", "   ", KindSite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyHost(tc.host, tc.domain); got != tc.want {
				t.Errorf("ClassifyHost(%q, %q) = %v, want %v", tc.host, tc.domain, got, tc.want)
			}
		})
	}
}

// TestClassifyHostFollowsTheFrontDoorHostSet holds the maintenance page's idea
// of "the platform's own hosts" to component/frontdoor, the one place the host
// set is derived: every host the front door serves, bar the sites wildcard, is
// the platform's. ClassifyHost reads that set instead of keeping a list of its
// own, so a platform site added there is branded here.
func TestClassifyHostFollowsTheFrontDoorHostSet(t *testing.T) {
	hosts := frontdoor.Hosts(testDomain)
	if len(hosts) < 6 {
		t.Fatalf("the front door composed %d hosts for %s; the guard would prove nothing", len(hosts), testDomain)
	}
	for _, h := range hosts {
		want := KindPlatform
		if h.Wildcard {
			// `*.<domain>` is the Ingress RULE that reaches every site, not a
			// host a request can carry.
			want = KindSite
		}
		if got := ClassifyHost(h.Name, testDomain); got != want {
			t.Errorf("front-door host %q classified %v, want %v", h.Name, got, want)
		}
	}
	if got := ClassifyHost(frontdoor.SiteHost("shop", testDomain), testDomain); got != KindSite {
		t.Errorf("a site the wildcard routes (shop.%s) classified %v, want site", testDomain, got)
	}
}

func TestPlatformPageIsBranded(t *testing.T) {
	rec := serve(t, testDomain, http.MethodGet, platformHost, "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "120" {
		t.Errorf("Retry-After = %q, want 120", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"MemQL", "<svg", "MemQL is being updated.", "It will be back in a few minutes.", platformMessage} {
		if !strings.Contains(body, want) {
			t.Errorf("the platform page does not contain %q", want)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("the platform page carries a script")
	}
}

// TestPlatformPageInlinesTheBrandMark reads the mark from brand/, not from the
// embedded copy, so a page that drew its own approximation of the logo fails
// here even while TestMarkMatchesBrand is green.
func TestPlatformPageInlinesTheBrandMark(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "brand", "mark.svg"))
	if err != nil {
		t.Fatalf("reading brand/mark.svg: %v", err)
	}
	elements := regexp.MustCompile(`<(?:line|circle)\b[^>]*/>`).FindAllString(string(src), -1)
	if len(elements) != 28 {
		t.Fatalf("brand/mark.svg has %d line and circle elements, expected 19 edges and 9 nodes; update the test if the mark changed", len(elements))
	}
	body := page(t, platformHost)
	for _, el := range elements {
		if !strings.Contains(body, el) {
			t.Errorf("the platform page is missing the mark element %s", el)
		}
	}
	if n := strings.Count(body, "<svg"); n != 1 {
		t.Errorf("the platform page has %d <svg> elements, want exactly the one mark", n)
	}
}

func TestSitePageIsNeutral(t *testing.T) {
	rec := serve(t, testDomain, http.MethodGet, siteHost, "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "120" {
		t.Errorf("Retry-After = %q, want 120", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	body := rec.Body.String()
	lower := strings.ToLower(body)
	for _, banned := range []string{"memql", "<svg", "<a ", "href"} {
		if strings.Contains(lower, banned) {
			t.Errorf("the site page contains %q; a customer's site must carry no MemQL mark, name or link", banned)
		}
	}
	for _, want := range []string{"This site is temporarily unavailable.", "It will be back soon.", siteMessage} {
		if !strings.Contains(body, want) {
			t.Errorf("the site page does not contain %q", want)
		}
	}
	// Nothing in the headers may name the platform either.
	for name, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(strings.ToLower(name+v), "memql") {
				t.Errorf("header %s: %s names the platform on a site response", name, v)
			}
		}
	}
}

func TestErrorPagesJSONForAPIClients(t *testing.T) {
	type envelope struct {
		Error   string `json:"error"`
		Status  int    `json:"status"`
		Message string `json:"message"`
	}
	cases := []struct {
		name    string
		host    string
		header  http.Header
		message string
	}{
		{"a site host asked for JSON", siteHost, http.Header{"Accept": {"application/json"}}, siteMessage},
		{"a platform host asked for JSON", platformHost, http.Header{"Accept": {"application/json"}}, platformMessage},
		{"the api host with no Accept", "api.memql.example", nil, platformMessage},
		{"the api host asked for HTML", "api.memql.example", http.Header{"Accept": {"text/html"}}, platformMessage},
		{"the api host in upper case with a port", "API.Memql.Example:443", nil, platformMessage},
		{"a client that lists JSON among others", siteHost, http.Header{"Accept": {"text/plain, application/json;q=0.9, */*;q=0.1"}}, siteMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, testDomain, http.MethodGet, tc.host, "/v1/anything", tc.header)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rec.Header().Get("Retry-After"); got != "120" {
				t.Errorf("Retry-After = %q, want 120", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"status":503`) {
				t.Errorf("body %s does not carry \"status\":503", body)
			}
			var env envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not JSON: %v\n%s", err, body)
			}
			if env.Error != "unavailable" || env.Status != 503 || env.Message != tc.message {
				t.Errorf("envelope = %+v, want error=unavailable status=503 message=%q", env, tc.message)
			}
			if tc.host == siteHost && strings.Contains(strings.ToLower(body), "memql") {
				t.Errorf("a site's JSON names the platform: %s", body)
			}
		})
	}
}

// TestBrowsersGetHTMLNotJSON pins the other side of the content negotiation: a
// browser's Accept header and an absent one are HTML, so a person is never
// shown a JSON blob.
func TestBrowsersGetHTMLNotJSON(t *testing.T) {
	for name, accept := range map[string]string{
		"a browser":       "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
		"a bare wildcard": "*/*",
		"no header":       "",
		"JSON refused":    "application/json;q=0, text/html",
		"a lookalike":     "application/problem+json",
	} {
		t.Run(name, func(t *testing.T) {
			header := http.Header{}
			if accept != "" {
				header.Set("Accept", accept)
			}
			rec := serve(t, testDomain, http.MethodGet, siteHost, "/", header)
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Accept %q got Content-Type %q, want text/html", accept, ct)
			}
		})
	}
}

func TestAcceptsJSON(t *testing.T) {
	for accept, want := range map[string]bool{
		"application/json":                  true,
		"Application/JSON":                  true,
		"application/json; charset=utf-8":   true,
		" application/json ":                true,
		"text/html, application/json;q=0.5": true,
		"application/json;q=0.001":          true,
		"application/json;q=0":              false,
		"application/json;q=0.0":            false,
		"application/*":                     false,
		"*/*":                               false,
		"text/html":                         false,
		"application/problem+json":          false,
		"application/jsonp":                 false,
		"":                                  false,
		";;;,,,":                            false,
		"application/json;q=not-a-number":   true,
		"text/html,application/xhtml+xml,*/*;q=0.8": false,
	} {
		if got := acceptsJSON(accept); got != want {
			t.Errorf("acceptsJSON(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestHealthz(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := serve(t, testDomain, method, "10.42.1.7:8080", "/healthz", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s /healthz answered %d, want 200", method, rec.Code)
		}
		if method == http.MethodGet && rec.Body.String() != "ok" {
			t.Errorf("GET /healthz body = %q, want ok", rec.Body.String())
		}
		if method == http.MethodHead && rec.Body.Len() != 0 {
			t.Errorf("HEAD /healthz wrote a body: %q", rec.Body.String())
		}
	}
}

// TestHealthzIsNotAnsweredForTheOutsideWorld is the false-green guard. This
// server is the default backend of every front door, so a request for
// `https://api.<domain>/healthz` that finds no endpoint reaches it too. The
// kubelet probes it directly and carries none of the headers ingress-nginx adds
// to every request it forwards; the outside world always does. If the forwarded
// request were answered 200 ok, `curl https://api.<domain>/healthz`, an uptime
// monitor and the post-sync endpoint probe would all read the middle of an
// outage as healthy. That is the one answer a maintenance page must not give.
func TestHealthzIsNotAnsweredForTheOutsideWorld(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Real-IP", "X-Request-ID", "Forwarded"} {
		t.Run(header, func(t *testing.T) {
			rec := serve(t, testDomain, http.MethodGet, "api.memql.example", "/healthz", http.Header{header: {"203.0.113.9"}})
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("a /healthz forwarded with %s answered %d, want 503", header, rec.Code)
			}
			if rec.Header().Get("Retry-After") != "120" {
				t.Errorf("Retry-After = %q, want 120", rec.Header().Get("Retry-After"))
			}
		})
	}
	// A POST to /healthz is not a probe whoever sends it.
	if rec := serve(t, testDomain, http.MethodPost, "10.42.1.7:8080", "/healthz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /healthz answered %d, want 503", rec.Code)
	}
	// Only the exact path is the probe.
	for _, target := range []string{"/healthz/", "/healthz/extra", "/Healthz", "/healthz.json"} {
		if rec := serve(t, testDomain, http.MethodGet, "10.42.1.7:8080", target, nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s answered %d, want 503", target, rec.Code)
		}
	}
}

func TestEveryPathAndMethodGets503(t *testing.T) {
	methods := []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	}
	targets := []string{"/", "/a/b/c", "/favicon.ico", "/robots.txt", "/healthz/", "/index.html?x=1", "/%20", "//"}
	for _, host := range []string{platformHost, siteHost, "api.memql.example", ""} {
		for _, method := range methods {
			for _, target := range targets {
				rec := serve(t, testDomain, method, host, target, nil)
				if rec.Code != http.StatusServiceUnavailable {
					t.Errorf("%s %s on %q answered %d, want 503", method, target, host, rec.Code)
				}
				if rec.Header().Get("Retry-After") != "120" || rec.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("%s %s on %q: Retry-After=%q Cache-Control=%q",
						method, target, host, rec.Header().Get("Retry-After"), rec.Header().Get("Cache-Control"))
				}
			}
		}
	}
}

// TestHeadAdvertisesTheSameLengthAndWritesNoBody: a HEAD probe from a monitor
// must see the headers a GET would, without the body.
func TestHeadAdvertisesTheSameLengthAndWritesNoBody(t *testing.T) {
	for _, host := range []string{platformHost, siteHost, "api.memql.example"} {
		get := serve(t, testDomain, http.MethodGet, host, "/", nil)
		head := serve(t, testDomain, http.MethodHead, host, "/", nil)
		if head.Body.Len() != 0 {
			t.Errorf("HEAD on %s wrote %d body bytes", host, head.Body.Len())
		}
		wantLen := strconv.Itoa(get.Body.Len())
		if got := get.Header().Get("Content-Length"); got != wantLen {
			t.Errorf("GET on %s: Content-Length = %q, want %s", host, got, wantLen)
		}
		if got := head.Header().Get("Content-Length"); got != wantLen {
			t.Errorf("HEAD on %s: Content-Length = %q, want %s", host, got, wantLen)
		}
		if head.Header().Get("Content-Type") != get.Header().Get("Content-Type") {
			t.Errorf("HEAD on %s: Content-Type differs from GET", host)
		}
	}
}

// TestResponsesVaryOnAccept: the same URL answers HTML or JSON by the Accept
// header, which a cache has to be told even though no-store keeps it from
// holding the answer.
func TestResponsesVaryOnAccept(t *testing.T) {
	for _, host := range []string{platformHost, siteHost, "api.memql.example"} {
		rec := serve(t, testDomain, http.MethodGet, host, "/", nil)
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept") {
			t.Errorf("host %s: Vary = %q, want it to name Accept", host, got)
		}
	}
}

func TestNoDomainMeansNoBranding(t *testing.T) {
	for _, host := range []string{platformHost, "memql.example", "api.memql.example", ""} {
		rec := serve(t, "", http.MethodGet, host, "/", http.Header{"Accept": {"text/html"}})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("host %q answered %d, want 503", host, rec.Code)
		}
		if strings.Contains(strings.ToLower(rec.Body.String()), "memql") {
			t.Errorf("with no domain configured, host %q was branded", host)
		}
	}
}

func TestHandlerNormalisesTheDomain(t *testing.T) {
	rec := serve(t, "  MemQL.Example.  ", http.MethodGet, "OS.MEMQL.EXAMPLE:443", "/", nil)
	if !strings.Contains(rec.Body.String(), "MemQL is being updated.") {
		t.Error("a domain configured with padding, upper case and a trailing dot did not brand its own hosts")
	}
}

// TestPagesAreSelfContained holds the pages to the rule that makes them safe to
// serve while everything else is down: nothing to fetch, nothing to run.
func TestPagesAreSelfContained(t *testing.T) {
	externalURL := regexp.MustCompile(`(?i)(?:https?:)?//[a-z0-9.-]+`)
	for name, host := range map[string]string{"platform": platformHost, "site": siteHost} {
		t.Run(name, func(t *testing.T) {
			body := page(t, host)
			if len(body) >= maxPageBytes {
				t.Errorf("the page is %d bytes; the budget is under %d", len(body), maxPageBytes)
			}
			for _, banned := range []string{
				"<script", "<link", "<img", "<iframe", "<object", "<embed", "<form", "<base",
				"@import", "url(", " src=", "href=", "@font-face",
			} {
				if strings.Contains(strings.ToLower(body), banned) {
					t.Errorf("the page contains %q; it must be fully self-contained", banned)
				}
			}
			// The one absolute URL a page may hold is the SVG namespace the mark
			// declares. It names no host anything fetches from.
			for _, u := range externalURL.FindAllString(body, -1) {
				if u != "http://www.w3.org" {
					t.Errorf("the page references %q", u)
				}
			}
			if n := strings.Count(body, "<style>"); n != 1 {
				t.Errorf("the page has %d <style> elements, want exactly one", n)
			}
			for i, r := range body {
				if r > unicode.MaxASCII {
					t.Errorf("non-ASCII character %q at byte %d; the pages carry no emoji or symbols", r, i)
					break
				}
			}
			if !strings.HasPrefix(body, "<!DOCTYPE html>") {
				t.Error("the page does not start with the HTML5 doctype")
			}
			for _, want := range []string{`<html lang="en">`, `name="viewport"`, `name="color-scheme"`, `prefers-color-scheme`} {
				if !strings.Contains(body, want) {
					t.Errorf("the page does not contain %q", want)
				}
			}
			// Motion is a courtesy, never a requirement: a page that animates
			// must also say how to hold still.
			if strings.Contains(body, "@keyframes") && !strings.Contains(body, "prefers-reduced-motion") {
				t.Error("the page animates but does not honour prefers-reduced-motion")
			}
		})
	}
}

// styleElement returns what is between the page's <style> tags.
func styleElement(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<style>")
	end := strings.Index(body, "</style>")
	if start < 0 || end < start {
		t.Fatalf("no <style> element in the page")
	}
	return body[start+len("<style>") : end]
}

// TestPagesPinTheirInlineStyleWithACSPHash makes "no external resource" a rule
// the browser enforces and not only a promise the test above checks: the policy
// allows nothing but this page's own stylesheet, by hash, so a page that grew a
// script or a remote font would be blocked rather than quietly working.
func TestPagesPinTheirInlineStyleWithACSPHash(t *testing.T) {
	for name, host := range map[string]string{"platform": platformHost, "site": siteHost} {
		t.Run(name, func(t *testing.T) {
			rec := serve(t, testDomain, http.MethodGet, host, "/", nil)
			sum := sha256.Sum256([]byte(styleElement(t, rec.Body.String())))
			wantStyle := "style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
			csp := rec.Header().Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'none'", wantStyle, "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("Content-Security-Policy %q lacks %q", csp, want)
				}
			}
			for _, banned := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
				if strings.Contains(csp, banned) {
					t.Errorf("Content-Security-Policy %q contains %q", csp, banned)
				}
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

// brandTokens reads the light/dark pair of every colour token in
// brand/tokens.css, by role: bg, surface, fg, fg-muted, border, accent, ...
func brandTokens(t *testing.T) map[string][2]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "brand", "tokens.css"))
	if err != nil {
		t.Fatalf("reading brand/tokens.css: %v", err)
	}
	// The header comment explains the tokens with examples of its own; only a
	// declaration counts.
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(src), "")
	re := regexp.MustCompile(`--memql-([a-z0-9-]+):\s*light-dark\(\s*(#[0-9a-fA-F]{6})\s*,\s*(#[0-9a-fA-F]{6})\s*\)`)
	tokens := map[string][2]string{}
	for _, m := range re.FindAllStringSubmatch(css, -1) {
		tokens[m[1]] = [2]string{strings.ToLower(m[2]), strings.ToLower(m[3])}
	}
	if len(tokens) < 6 {
		t.Fatalf("read %d colour tokens from brand/tokens.css; the format changed under this test", len(tokens))
	}
	return tokens
}

// TestPalettesMatchBrandTokens is the drift guard for the colours. The pages
// cannot import brand/tokens.css (nothing may be fetched), and a page that
// cannot say "memql" cannot even reuse the token names, so the palette is
// written out twice: here it is held to the one source. brand_shared_source_test
// exists because two copies of a palette are two palettes; this is the same
// rule for the copy the brand's own build cannot reach.
func TestPalettesMatchBrandTokens(t *testing.T) {
	tokens := brandTokens(t)
	// page variable -> brand role
	roles := map[string]string{
		"bg": "bg", "surface": "surface", "fg": "fg", "muted": "fg-muted", "line": "border", "accent": "accent",
	}
	decl := regexp.MustCompile(`--([a-z-]+):\s*(#[0-9a-fA-F]{6})`)
	for name, host := range map[string]string{"platform": platformHost, "site": siteHost} {
		t.Run(name, func(t *testing.T) {
			body := page(t, host)
			declared := map[string][]string{}
			var order []string
			for _, m := range decl.FindAllStringSubmatch(styleElement(t, body), -1) {
				if _, seen := declared[m[1]]; !seen {
					order = append(order, m[1])
				}
				declared[m[1]] = append(declared[m[1]], strings.ToLower(m[2]))
			}
			for _, must := range []string{"bg", "surface", "fg", "muted", "line"} {
				if _, ok := declared[must]; !ok {
					t.Errorf("the page declares no --%s", must)
				}
			}
			for _, v := range order {
				role, ok := roles[v]
				if !ok {
					t.Errorf("--%s is not a palette variable this test maps to a brand token", v)
					continue
				}
				want := tokens[role]
				got := declared[v]
				if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
					t.Errorf("--%s = %v (light, dark); brand --memql-%s is %v", v, got, role, want)
				}
			}
			if name == "site" {
				if _, ok := declared["accent"]; ok {
					t.Error("the site page uses the MemQL accent colour; a neutral page wears none of the brand")
				}
			}
			// The browser chrome follows the same ground.
			for scheme, idx := range map[string]int{"light": 0, "dark": 1} {
				re := regexp.MustCompile(`name="theme-color"\s+media="\(prefers-color-scheme:\s*` + scheme + `\)"\s+content="(#[0-9a-fA-F]{6})"`)
				m := re.FindStringSubmatch(body)
				if m == nil {
					t.Errorf("no %s theme-color meta", scheme)
					continue
				}
				if want := tokens["bg"][idx]; strings.ToLower(m[1]) != want {
					t.Errorf("%s theme-color = %s, want brand bg %s", scheme, m[1], want)
				}
			}
		})
	}
}

// TestMarkMatchesBrand pins the one deliberate copy. //go:embed cannot reach
// ../../brand, so the mark is copied beside the package; the copy drifting is a
// build failure, as the identity favicon copy is (brand_shared_source_test.go).
func TestMarkMatchesBrand(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "brand", "mark.svg"))
	if err != nil {
		t.Fatalf("reading brand/mark.svg: %v", err)
	}
	copied, err := os.ReadFile("mark.svg")
	if err != nil {
		t.Fatalf("reading component/errorpages/mark.svg: %v", err)
	}
	if string(source) != string(copied) {
		t.Errorf("component/errorpages/mark.svg has drifted from brand/mark.svg.\n" +
			"Copy the brand file over it: cp brand/mark.svg component/errorpages/mark.svg")
	}
}
