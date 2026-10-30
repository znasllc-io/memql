package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// access_test.go -- THE EDGE HALF OF THE ACCESS SURFACE (deployable access
// codes, docs/superpowers/specs/2026-10-29-deployable-access-codes-design.md,
// sections 6 and 9).
//
// Every test here drives the real Handler against a fake bff, as shopper_test.go
// does, and asserts what the fake RECEIVED. A refusal is proved by the bff
// seeing nothing, and a forward by the bff seeing the stamp and the rewritten
// path -- never by a status code alone, because a 404 from the wrong branch
// looks the same as a 404 from the right one.

// accessHit is one request the fake bff received.
type accessHit struct {
	method           string
	path             string
	header           http.Header
	body             string
	contentLength    int64
	transferEncoding []string
}

// accessUpstream stands in for the bff. It records every request and answers
// with whatever the test set, or an unremarkable 200 that carries NO caching
// header, so a test about the edge's own headers cannot pass on the bff's.
type accessUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   []accessHit
	reply  func(w http.ResponseWriter, r *http.Request)
}

func newAccessUpstream(t *testing.T) *accessUpstream {
	t.Helper()
	up := &accessUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.hits = append(up.hits, accessHit{
			method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(body),
			contentLength: r.ContentLength, transferEncoding: r.TransferEncoding,
		})
		reply := up.reply
		up.mu.Unlock()
		if reply != nil {
			reply(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"state":"ok"}`)
	}))
	t.Cleanup(up.server.Close)
	return up
}

func (u *accessUpstream) answer(reply func(w http.ResponseWriter, r *http.Request)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reply = reply
}

func (u *accessUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.hits)
}

// only returns the one request the bff received and fails if there was any
// other number, so a test that reads a header cannot be reading the wrong hit.
func (u *accessUpstream) only(t *testing.T) accessHit {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.hits) != 1 {
		t.Fatalf("the bff received %d requests, want exactly 1", len(u.hits))
	}
	return u.hits[0]
}

// accessClock is the limiter's clock. The buckets refill continuously, so a
// test that spends a whole budget against the wall clock would be asserting
// against a refill it did not control; frozen, every count here is exact.
type accessClock struct{ now time.Time }

func (c *accessClock) Now() time.Time          { return c.now }
func (c *accessClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// accessHostResolver resolves by Host, for the tests that need two sites.
type accessHostResolver map[string]*Site

func (m accessHostResolver) Resolve(_ context.Context, host string) (*Site, error) {
	return m[host], nil
}
func (m accessHostResolver) Invalidate(string) {}
func (m accessHostResolver) InvalidateAll()    {}

// accessSite is a live, owned deployable with apiProxy OFF -- the ordinary
// shape of a gated site, and the one that proves the route does not borrow the
// proxy's switch.
func accessSite(mutate func(*Site)) *Site {
	s := &Site{
		ID: "site1", Hostname: "demo.example.com", Status: "live", Kind: "spa",
		OwnerUserID: "user-owner",
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func accessHandlerFor(t *testing.T, resolver Resolver, target string) (*Handler, *accessClock) {
	t.Helper()
	h := NewHandler(Options{
		Resolver:  resolver,
		Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		APITarget: target,
	})
	clock := &accessClock{now: time.Unix(1_800_000_000, 0)}
	h.accessRate.now = clock.Now
	return h, clock
}

func accessHandler(t *testing.T, site *Site, target string) *Handler {
	t.Helper()
	h, _ := accessHandlerFor(t, staticResolver{site: site}, target)
	return h
}

// accessRequest builds a visitor's request. A POST carries the JSON content
// type a real client sends; the tests that need it wrong set it themselves.
func accessRequest(method, path, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Host = "demo.example.com"
	req.RemoteAddr = "198.51.100.7:40000"
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// accessUnlock is an unlock from a given claimed address.
func accessUnlock(addr string) *http.Request {
	req := accessRequest(http.MethodPost, "/_memql/access", `{"code":"x"}`)
	if addr != "" {
		req.Header.Set("X-Forwarded-For", addr)
	}
	return req
}

func serveAccess(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// accessErrorCode reads the {"error": ...} an edge refusal carries.
func accessErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the reply is not the JSON the protocol speaks: %q (%v)", rec.Body.String(), err)
	}
	return body.Error
}

// mustRetryAfter reads a Retry-After the edge sent: whole seconds, at least
// one, and no more than the minute a bucket needs to refill completely.
func mustRetryAfter(t *testing.T, rec *httptest.ResponseRecorder) int {
	t.Helper()
	raw := rec.Header().Get("Retry-After")
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 1 || secs > 60 {
		t.Fatalf("Retry-After = %q, want whole seconds between 1 and 60", raw)
	}
	return secs
}

// accessPairs are the four method and path pairs and where each one lands.
var accessPairs = []struct {
	name, method, path, forwarded string
}{
	{"unlock", http.MethodPost, "/_memql/access", "/access/unlock"},
	{"session", http.MethodGet, "/_memql/access", "/access/session"},
	{"leave", http.MethodPost, "/_memql/access/leave", "/access/leave"},
	{"call", http.MethodPost, "/_memql/access/call", "/access/call"},
}

// THE ROUTE IS FOUR PAIRS AND NOTHING ELSE. The interesting rows are the ones
// that look like it: the right path with the wrong method, the bff's own
// spelling, a longer segment, and a path that merely starts with the same
// letters. Each must reach the generic proxy untouched -- with apiProxy on it
// is forwarded under the "/memql" root with no stamp, and with it off it is
// the 404 it always was -- which is what proves it did not enter this surface.
func TestAccessRoutesFourPairsAndNothingElse(t *testing.T) {
	for _, pair := range accessPairs {
		t.Run("routed/"+pair.name, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(func(s *Site) { s.APIProxy = true }), up.server.URL)

			rec := serveAccess(h, accessRequest(pair.method, pair.path, ""))

			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200", pair.method, pair.path, rec.Code)
			}
			hit := up.only(t)
			if hit.method != pair.method || hit.path != pair.forwarded {
				t.Fatalf("the bff received %s %s, want %s %s",
					hit.method, hit.path, pair.method, pair.forwarded)
			}
			if hit.header.Get(AccessStampSiteHeader) != "site1" || hit.header.Get(AccessStampOwnerHeader) != "user-owner" {
				t.Fatalf("the stamp is site=%q owner=%q, want site1/user-owner",
					hit.header.Get(AccessStampSiteHeader), hit.header.Get(AccessStampOwnerHeader))
			}
		})
	}

	// want is what the generic proxy forwards a path as, spelled out rather than
	// computed with upstreamPath so the table does not assert the function
	// against itself.
	notRouted := []struct {
		name, method, path, want string
	}{
		{"DELETE on the unlock path", http.MethodDelete, "/_memql/access", "/memql/access"},
		{"PUT on the unlock path", http.MethodPut, "/_memql/access", "/memql/access"},
		{"PATCH on the unlock path", http.MethodPatch, "/_memql/access", "/memql/access"},
		{"HEAD on the session path", http.MethodHead, "/_memql/access", "/memql/access"},
		{"OPTIONS on the unlock path", http.MethodOptions, "/_memql/access", "/memql/access"},
		{"GET on leave", http.MethodGet, "/_memql/access/leave", "/memql/access/leave"},
		{"GET on call", http.MethodGet, "/_memql/access/call", "/memql/access/call"},
		{"DELETE on leave", http.MethodDelete, "/_memql/access/leave", "/memql/access/leave"},
		{"the bff's own name for unlock", http.MethodPost, "/_memql/access/unlock", "/memql/access/unlock"},
		{"the bff's own name for session", http.MethodGet, "/_memql/access/session", "/memql/access/session"},
		{"POST to a segment of its own", http.MethodPost, "/_memql/access/x", "/memql/access/x"},
		{"GET to a segment of its own", http.MethodGet, "/_memql/access/x", "/memql/access/x"},
		{"a trailing slash on unlock", http.MethodPost, "/_memql/access/", "/memql/access/"},
		{"a trailing slash on leave", http.MethodPost, "/_memql/access/leave/", "/memql/access/leave/"},
		{"a segment below call", http.MethodPost, "/_memql/access/call/x", "/memql/access/call/x"},
		{"POST to a longer first segment", http.MethodPost, "/_memql/accessories", "/memql/accessories"},
		{"GET to a longer first segment", http.MethodGet, "/_memql/accessories", "/memql/accessories"},
	}
	for _, tc := range notRouted {
		t.Run("not routed/"+tc.name+"/apiProxy on", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(func(s *Site) { s.APIProxy = true }), up.server.URL)

			serveAccess(h, accessRequest(tc.method, tc.path, ""))

			hit := up.only(t)
			if hit.path != tc.want {
				t.Fatalf("%s %s reached the bff as %q, want %q from the generic proxy",
					tc.method, tc.path, hit.path, tc.want)
			}
			if hit.header.Get(AccessStampSiteHeader) != "" || hit.header.Get(AccessStampOwnerHeader) != "" {
				t.Fatalf("%s %s was stamped: it entered the access surface", tc.method, tc.path)
			}
		})
		t.Run("not routed/"+tc.name+"/apiProxy off", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(nil), up.server.URL)

			rec := serveAccess(h, accessRequest(tc.method, tc.path, ""))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want the generic proxy's 404", tc.method, tc.path, rec.Code)
			}
			if up.count() != 0 {
				t.Fatalf("%s %s reached the bff", tc.method, tc.path)
			}
			if strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("%s %s got the access surface's JSON refusal, so it entered that surface: %q",
					tc.method, tc.path, rec.Body.String())
			}
		})
	}
}

// THE ROUTE DOES NOT BORROW THE API PROXY'S SWITCH. That switch is for a bundle
// holding a MemQL cookie; a visitor with an access code holds none, and a gated
// site that wants its gate and no authenticated API says exactly that.
func TestAccessIgnoresAPIProxy(t *testing.T) {
	for _, apiProxy := range []bool{false, true} {
		for _, pair := range accessPairs {
			t.Run(fmt.Sprintf("apiProxy=%v/%s", apiProxy, pair.name), func(t *testing.T) {
				up := newAccessUpstream(t)
				h := accessHandler(t, accessSite(func(s *Site) { s.APIProxy = apiProxy }), up.server.URL)

				rec := serveAccess(h, accessRequest(pair.method, pair.path, ""))

				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s = %d, want 200 whatever apiProxy says", pair.method, pair.path, rec.Code)
				}
				if hit := up.only(t); hit.path != pair.forwarded {
					t.Fatalf("the bff received %q, want %q", hit.path, pair.forwarded)
				}
			})
		}
	}

	// The control: with apiProxy off the generic proxy really is off on this
	// fixture, so the passes above are not an artifact of how the site is built.
	up := newAccessUpstream(t)
	h := accessHandler(t, accessSite(nil), up.server.URL)
	if rec := serveAccess(h, accessRequest(http.MethodGet, "/_memql/ws", "")); rec.Code != http.StatusNotFound || up.count() != 0 {
		t.Fatalf("the fixture's apiProxy is not off: GET /_memql/ws = %d with %d upstream hits", rec.Code, up.count())
	}
}

// A CLIENT-SUPPLIED STAMP DOES NOT SURVIVE. The bff treats the headers as a
// pointer it then verifies, but only because the edge is the sole author of
// them; a forged copy that rode through would make the pointer an assertion.
// Both headers are checked with a SECOND forged value as well, since Del drops
// every value and Set replaces them, and a refactor to Add would keep the first.
func TestAccessStripsThenStamps(t *testing.T) {
	for _, pair := range accessPairs {
		t.Run(pair.name, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(nil), up.server.URL)

			req := accessRequest(pair.method, pair.path, "")
			req.Header.Set(AccessStampSiteHeader, "site-someone-elses")
			req.Header.Add(AccessStampSiteHeader, "site-another")
			req.Header.Set(AccessStampOwnerHeader, "user-someone-else")
			req.Header.Add(AccessStampOwnerHeader, "user-another")
			serveAccess(h, req)

			hit := up.only(t)
			if got := hit.header.Values(AccessStampSiteHeader); len(got) != 1 || got[0] != "site1" {
				t.Fatalf("stamped site = %q, want exactly [site1]", got)
			}
			if got := hit.header.Values(AccessStampOwnerHeader); len(got) != 1 || got[0] != "user-owner" {
				t.Fatalf("stamped owner = %q, want exactly [user-owner]", got)
			}
		})
	}
}

// THE TWO NAMES ARE A CONTRACT WITH A PACKAGE THIS ONE CANNOT IMPORT.
// component/server reads them, and this file may not import it, so the literals
// are pinned here: a rename on either side that did not take the other with it
// would make the bff answer every visit as an unstamped request, and nothing
// anywhere would name the cause.
func TestAccessStampHeaderNamesArePinned(t *testing.T) {
	if AccessStampSiteHeader != "X-Memql-Access-Site" {
		t.Fatalf("AccessStampSiteHeader = %q, want X-Memql-Access-Site", AccessStampSiteHeader)
	}
	if AccessStampOwnerHeader != "X-Memql-Access-Owner" {
		t.Fatalf("AccessStampOwnerHeader = %q, want X-Memql-Access-Owner", AccessStampOwnerHeader)
	}
}

// A CLUSTER-OWNED SITE HAS NO AUTHORITY TO BORROW. The platform's own sites are
// the population, and a call would run as nobody. The reply is the very 404 the
// bff gives an unknown route, so an owner-less site is not told apart from a
// site that has nothing here.
func TestAccessRefusesASiteWithNoOwner(t *testing.T) {
	for _, owner := range []string{"", "   "} {
		for _, pair := range accessPairs {
			t.Run(fmt.Sprintf("owner=%q/%s", owner, pair.name), func(t *testing.T) {
				up := newAccessUpstream(t)
				h := accessHandler(t, accessSite(func(s *Site) { s.OwnerUserID = owner }), up.server.URL)

				rec := serveAccess(h, accessRequest(pair.method, pair.path, ""))

				if rec.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404", rec.Code)
				}
				if got := rec.Body.String(); got != `{"error":"not_found"}` {
					t.Fatalf("body = %q, want the bff's own not_found, byte for byte", got)
				}
				if up.count() != 0 {
					t.Fatal("a site with no owner must not reach the bff")
				}
			})
		}
	}
}

// CROSS-SITE FORGERY, the third leg: SameSite=Strict keeps the cookie off a
// cross-site request and the JSON-only rule keeps a plain form from sending
// one, and this refuses the browser-made request that names another origin.
// ABSENT IS ALLOWED, because a request with no Origin is not one a browser made
// cross-site; and the comparison is against the REQUEST'S host, not the
// site's, because a custom domain reaches the same site under its own name.
func TestAccessRefusesAForeignOrigin(t *testing.T) {
	posts := []struct{ name, path string }{
		{"unlock", "/_memql/access"},
		{"leave", "/_memql/access/leave"},
		{"call", "/_memql/access/call"},
	}
	refused := []struct{ name, host, origin string }{
		{"another site", "demo.example.com", "https://evil.example"},
		{"a suffix lookalike", "demo.example.com", "https://demo.example.com.evil.example"},
		{"a prefix lookalike", "demo.example.com", "https://evildemo.example.com"},
		{"a sibling subdomain", "demo.example.com", "https://other.example.com"},
		{"the same host on another port", "demo.example.com", "https://demo.example.com:8443"},
		{"an opaque origin", "demo.example.com", "null"},
		{"something that is not an origin", "demo.example.com", "not a url"},
		{"an empty header", "demo.example.com", ""},
		{"the site's own host, asked for through a custom domain", "docs.customer.example", "https://demo.example.com"},
	}
	for _, post := range posts {
		for _, tc := range refused {
			t.Run("refused/"+post.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := accessHandler(t, accessSite(nil), up.server.URL)

				req := accessRequest(http.MethodPost, post.path, "")
				req.Host = tc.host
				req.Header.Set("Origin", tc.origin)
				rec := serveAccess(h, req)

				if rec.Code != http.StatusForbidden {
					t.Fatalf("Origin %q on host %q = %d, want 403", tc.origin, tc.host, rec.Code)
				}
				if up.count() != 0 {
					t.Fatalf("Origin %q reached the bff", tc.origin)
				}
			})
		}
	}

	allowed := []struct{ name, host, origin string }{
		{"no origin at all", "demo.example.com", "-"},
		{"its own origin", "demo.example.com", "https://demo.example.com"},
		{"its own origin in another case", "demo.example.com", "https://DEMO.example.com"},
		{"its own origin on a port", "localhost:8080", "http://localhost:8080"},
		{"a custom domain under its own name", "docs.customer.example", "https://docs.customer.example"},
	}
	for _, post := range posts {
		for _, tc := range allowed {
			t.Run("allowed/"+post.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				// The site's own hostname is NOT the request's here, which is the
				// custom-domain case: the resolver hands back the same site.
				h := accessHandler(t, accessSite(nil), up.server.URL)

				req := accessRequest(http.MethodPost, post.path, "")
				req.Host = tc.host
				if tc.origin != "-" {
					req.Header.Set("Origin", tc.origin)
				}
				rec := serveAccess(h, req)

				if rec.Code != http.StatusOK || up.count() != 1 {
					t.Fatalf("Origin %q on host %q = %d with %d upstream hits, want it forwarded",
						tc.origin, tc.host, rec.Code, up.count())
				}
			})
		}
	}
}

// JSON-ONLY ON POST. A plain HTML form can send only urlencoded, multipart or
// text/plain, and a script that sends anything else is held by the browser's
// preflight, so a body that is not declared JSON is not one a cross-site page
// could have sent. A GET has no body and is not asked.
func TestAccessRequiresJSONOnPost(t *testing.T) {
	refused := []struct{ name, contentType string }{
		{"none", ""},
		{"text/plain", "text/plain"},
		{"a form", "application/x-www-form-urlencoded"},
		{"multipart", "multipart/form-data; boundary=x"},
		{"a longer subtype", "application/jsonx"},
		{"a structured suffix", "application/vnd.api+json"},
		{"json as a parameter", "text/plain; type=application/json"},
		{"something unparseable", "application/json, text/plain"},
	}
	for _, pair := range accessPairs {
		if pair.method != http.MethodPost {
			continue
		}
		for _, tc := range refused {
			t.Run("refused/"+pair.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := accessHandler(t, accessSite(nil), up.server.URL)

				req := accessRequest(pair.method, pair.path, `{"code":"x"}`)
				req.Header.Del("Content-Type")
				if tc.contentType != "" {
					req.Header.Set("Content-Type", tc.contentType)
				}
				rec := serveAccess(h, req)

				if rec.Code != http.StatusUnsupportedMediaType {
					t.Fatalf("Content-Type %q = %d, want 415", tc.contentType, rec.Code)
				}
				if up.count() != 0 {
					t.Fatalf("Content-Type %q reached the bff", tc.contentType)
				}
			})
		}
		for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
			t.Run("allowed/"+pair.name+"/"+contentType, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := accessHandler(t, accessSite(nil), up.server.URL)

				req := accessRequest(pair.method, pair.path, `{"code":"x"}`)
				req.Header.Set("Content-Type", contentType)
				rec := serveAccess(h, req)

				if rec.Code != http.StatusOK || up.count() != 1 {
					t.Fatalf("Content-Type %q = %d with %d upstream hits, want it forwarded",
						contentType, rec.Code, up.count())
				}
			})
		}
	}

	t.Run("a GET is not asked", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		rec := serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", ""))

		if rec.Code != http.StatusOK || up.count() != 1 {
			t.Fatalf("GET /_memql/access = %d with %d upstream hits, want it forwarded", rec.Code, up.count())
		}
	})
}

// THE CAPS: 1 KiB for unlock and leave, 64 KiB for call, refused before the
// hop. The cap is on what is READ, not on what the request claims, so a body
// with no declared length is held to it as well; and a cap is only worth
// asserting if the body at exactly the limit still gets through whole.
func TestAccessCapsBodies(t *testing.T) {
	caps := []struct {
		name, method, path string
		limit              int
	}{
		{"unlock", http.MethodPost, "/_memql/access", 1 << 10},
		{"leave", http.MethodPost, "/_memql/access/leave", 1 << 10},
		{"call", http.MethodPost, "/_memql/access/call", 64 << 10},
		{"session", http.MethodGet, "/_memql/access", 1 << 10},
	}
	for _, tc := range caps {
		t.Run(tc.name+"/exactly the cap is forwarded whole", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(nil), up.server.URL)
			body := strings.Repeat("a", tc.limit)

			rec := serveAccess(h, accessRequest(tc.method, tc.path, body))

			if rec.Code != http.StatusOK {
				t.Fatalf("%d bytes = %d, want 200", tc.limit, rec.Code)
			}
			if got := up.only(t).body; got != body {
				t.Fatalf("the bff received %d bytes, want the %d it was sent", len(got), len(body))
			}
		})
		t.Run(tc.name+"/one byte over is refused before forwarding", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(nil), up.server.URL)

			rec := serveAccess(h, accessRequest(tc.method, tc.path, strings.Repeat("a", tc.limit+1)))

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%d bytes = %d, want 413", tc.limit+1, rec.Code)
			}
			if got := accessErrorCode(t, rec); got != "too_large" {
				t.Fatalf("error = %q, want too_large", got)
			}
			if up.count() != 0 {
				t.Fatal("an oversize body reached the bff")
			}
		})
		t.Run(tc.name+"/a body with no declared length is capped while it is read", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(nil), up.server.URL)

			// A reader the standard library does not recognise leaves
			// ContentLength at -1, which is what a chunked upload looks like.
			req := accessRequest(tc.method, tc.path, "")
			req.Body = io.NopCloser(strings.NewReader(strings.Repeat("a", tc.limit+1)))
			req.ContentLength = -1
			rec := serveAccess(h, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("an undeclared %d bytes = %d, want 413", tc.limit+1, rec.Code)
			}
			if up.count() != 0 {
				t.Fatal("an undeclared oversize body reached the bff")
			}
		})
		t.Run(tc.name+"/a body with no declared length within the cap is forwarded whole", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := accessHandler(t, accessSite(nil), up.server.URL)
			body := strings.Repeat("a", tc.limit)

			req := accessRequest(tc.method, tc.path, "")
			req.Body = io.NopCloser(strings.NewReader(body))
			req.ContentLength = -1
			rec := serveAccess(h, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("an undeclared %d bytes = %d, want 200", tc.limit, rec.Code)
			}
			if got := up.only(t).body; got != body {
				t.Fatalf("the bff received %d bytes, want the %d it was sent", len(got), len(body))
			}
		})
	}

	// The two caps are two caps: a call may carry what an unlock may not.
	t.Run("a call takes what an unlock refuses", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)
		body := strings.Repeat("a", 2<<10)

		unlock := serveAccess(h, accessRequest(http.MethodPost, "/_memql/access", body))
		call := serveAccess(h, accessRequest(http.MethodPost, "/_memql/access/call", body))

		if unlock.Code != http.StatusRequestEntityTooLarge || call.Code != http.StatusOK {
			t.Fatalf("2 KiB: unlock = %d, call = %d, want 413 and 200", unlock.Code, call.Code)
		}
	})
}

// A CHUNKED UPLOAD REACHES THE BFF WITH A LENGTH. The edge reads the body to
// enforce the cap, so it knows how long it is and says so; the bff is not handed
// a stream it was told to expect in pieces. This one goes over a real
// connection, because httptest.NewRequest never sets the field a chunked
// request carries and so cannot tell whether the edge cleared it.
func TestAccessGivesTheBffABodyOfKnownLength(t *testing.T) {
	up := newAccessUpstream(t)
	h := accessHandler(t, accessSite(nil), up.server.URL)
	front := httptest.NewServer(h)
	defer front.Close()

	body := strings.Repeat("a", 700)
	req, err := http.NewRequest(http.MethodPost, front.URL+"/_memql/access/call",
		io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // unknown, so the client sends it chunked
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	hit := up.only(t)
	if hit.body != body {
		t.Fatalf("the bff received %d bytes, want the %d it was sent", len(hit.body), len(body))
	}
	if hit.contentLength != int64(len(body)) || len(hit.transferEncoding) != 0 {
		t.Fatalf("the bff received Content-Length %d with Transfer-Encoding %q, want %d and none",
			hit.contentLength, hit.transferEncoding, len(body))
	}
}

// UNLOCK: 10 PER MINUTE PER (SITE, ADDRESS), AND 60 PER MINUTE PER SITE. The
// address is the leftmost X-Forwarded-For, which a client can write, and that
// is why the second bucket exists: spoofing 60 addresses spends it, and past
// that nobody gets in until it refills. These bound load, not guessing.
func TestAccessRateLimitsUnlockPerAddressAndPerSite(t *testing.T) {
	t.Run("ten per address", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		for i := 0; i < 10; i++ {
			if rec := serveAccess(h, accessUnlock("203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("unlock %d = %d, want 200 inside the budget", i+1, rec.Code)
			}
		}
		rec := serveAccess(h, accessUnlock("203.0.113.9"))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the 11th unlock = %d, want 429", rec.Code)
		}
		mustRetryAfter(t, rec)
		if got := accessErrorCode(t, rec); got != "rate_limited" {
			t.Fatalf("error = %q, want rate_limited", got)
		}

		// The leftmost entry is the address; what a hop appended is not.
		if rec := serveAccess(h, accessUnlock("203.0.113.9, 10.0.0.1")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the same address with a hop appended = %d, want the same bucket", rec.Code)
		}
		if rec := serveAccess(h, accessUnlock("203.0.113.10")); rec.Code != http.StatusOK {
			t.Fatalf("another address = %d, want its own budget", rec.Code)
		}
		if up.count() != 11 {
			t.Fatalf("the bff received %d unlocks, want 10 + 1 from the other address", up.count())
		}
	})

	t.Run("with no forwarded address the peer is the address", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		for i := 0; i < 10; i++ {
			serveAccess(h, accessUnlock(""))
		}
		if rec := serveAccess(h, accessUnlock("")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the 11th from one peer = %d, want 429", rec.Code)
		}
		other := accessUnlock("")
		other.RemoteAddr = "198.51.100.8:40000"
		if rec := serveAccess(h, other); rec.Code != http.StatusOK {
			t.Fatalf("another peer = %d, want its own budget", rec.Code)
		}
	})

	t.Run("sixty per site, whatever addresses are claimed", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		for i := 0; i < 60; i++ {
			if rec := serveAccess(h, accessUnlock(fmt.Sprintf("203.0.113.%d", i))); rec.Code != http.StatusOK {
				t.Fatalf("unlock %d from its own address = %d, want 200 inside the site's budget", i+1, rec.Code)
			}
		}
		// A 61st from an address that has spent nothing: its own bucket is full,
		// so this is the SITE'S refusal.
		rec := serveAccess(h, accessUnlock("198.18.0.1"))
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the 61st unlock for the site = %d, want 429", rec.Code)
		}
		mustRetryAfter(t, rec)
		if up.count() != 60 {
			t.Fatalf("the bff received %d unlocks, want 60", up.count())
		}
		// Only unlock is spent: a status poll still goes through.
		if rec := serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", "")); rec.Code != http.StatusOK {
			t.Fatalf("a poll after the site's unlock budget is gone = %d, want 200", rec.Code)
		}
	})

	// THE ORDER OF THE TWO BUCKETS IS THE POINT OF THIS CASE. One address that
	// floods must be stopped by its OWN bucket before it reaches the site's, or
	// a single machine drains the whole site's budget in a second and nobody
	// else can get in. Spent site-first, A's hundred requests would take all
	// sixty and leave nothing for the fifty that follow.
	t.Run("one address cannot spend the site's budget", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		passed := 0
		for i := 0; i < 100; i++ {
			if serveAccess(h, accessUnlock("203.0.113.9")).Code == http.StatusOK {
				passed++
			}
		}
		if passed != 10 {
			t.Fatalf("a flooding address got %d through, want its own 10", passed)
		}
		for i := 0; i < 50; i++ {
			if rec := serveAccess(h, accessUnlock(fmt.Sprintf("198.18.1.%d", i))); rec.Code != http.StatusOK {
				t.Fatalf("visitor %d after the flood = %d, want 200: the flood spent the site's budget", i+1, rec.Code)
			}
		}
		if rec := serveAccess(h, accessUnlock("198.18.2.1")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the visitor after the site's 60 = %d, want 429", rec.Code)
		}
		if up.count() != 60 {
			t.Fatalf("the bff received %d unlocks, want 10 + 50", up.count())
		}
	})

	t.Run("each site has its own budgets", func(t *testing.T) {
		up := newAccessUpstream(t)
		h, _ := accessHandlerFor(t, accessHostResolver{
			"demo.example.com":  accessSite(nil),
			"other.example.com": accessSite(func(s *Site) { s.ID = "site2"; s.Hostname = "other.example.com" }),
		}, up.server.URL)

		for i := 0; i < 10; i++ {
			serveAccess(h, accessUnlock("203.0.113.9"))
		}
		if rec := serveAccess(h, accessUnlock("203.0.113.9")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the 11th on the first site = %d, want 429", rec.Code)
		}
		second := accessUnlock("203.0.113.9")
		second.Host = "other.example.com"
		if rec := serveAccess(h, second); rec.Code != http.StatusOK {
			t.Fatalf("the same address on another site = %d, want that site's own budget", rec.Code)
		}
	})

	t.Run("the budget comes back", func(t *testing.T) {
		up := newAccessUpstream(t)
		h, clock := accessHandlerFor(t, staticResolver{site: accessSite(nil)}, up.server.URL)

		for i := 0; i < 10; i++ {
			serveAccess(h, accessUnlock("203.0.113.9"))
		}
		if rec := serveAccess(h, accessUnlock("203.0.113.9")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the 11th = %d, want 429", rec.Code)
		}
		// 10 a minute is one every six seconds. 6.5 s is a token and a half, so
		// the count does not hang on a rounding at the boundary.
		clock.advance(6500 * time.Millisecond)
		if rec := serveAccess(h, accessUnlock("203.0.113.9")); rec.Code != http.StatusOK {
			t.Fatalf("after 6.5 s = %d, want one unlock back", rec.Code)
		}
		if rec := serveAccess(h, accessUnlock("203.0.113.9")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("a second after 6.5 s = %d, want only the one token", rec.Code)
		}
	})
}

// EVERYTHING BUT UNLOCK IS A SEPARATE, LARGER BUDGET. A page that polls its
// session every few seconds, a call per click and a leave are ordinary use,
// and none of it may spend the budget that guards a guessing attempt: the
// reverse matters just as much, so a visitor who mistypes a code ten times is
// not also locked out of the page they are already looking at.
func TestAccessRateLimitsOtherOperationsSeparately(t *testing.T) {
	t.Run("a burst of status polls does not spend unlock's budget", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		// More than unlock's whole budget, so a shared bucket would be empty.
		for i := 0; i < 25; i++ {
			if rec := serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", "")); rec.Code != http.StatusOK {
				t.Fatalf("poll %d = %d, want 200", i+1, rec.Code)
			}
		}
		for i := 0; i < 10; i++ {
			if rec := serveAccess(h, accessUnlock("")); rec.Code != http.StatusOK {
				t.Fatalf("unlock %d after the polls = %d, want 200: the polls spent its budget", i+1, rec.Code)
			}
		}
		if rec := serveAccess(h, accessUnlock("")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the 11th unlock = %d, want 429", rec.Code)
		}
	})

	t.Run("an unlock budget that is gone does not stop the page", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		for i := 0; i < 11; i++ {
			serveAccess(h, accessUnlock(""))
		}
		for _, pair := range accessPairs[1:] {
			if rec := serveAccess(h, accessRequest(pair.method, pair.path, "")); rec.Code != http.StatusOK {
				t.Fatalf("%s after unlock was refused = %d, want 200", pair.name, rec.Code)
			}
		}
	})

	// The 600 a minute ceiling, and that session, leave and call share it, is
	// asserted exactly in TestAccessLimitsKeyAndRefill, where a token costs no
	// request. Here the claim is only that the budgets are not the same budget.
}

// EVERY REPLY CARRIES no-store AND nosniff, the bff's and the edge's own. No
// cache between here and a browser may hold an answer to "are you still in",
// and a reply the bff forgot to mark, or marked cacheable, must not depend on
// the bff getting it right. EXACTLY ONE of each: the proxy ADDS the upstream's
// headers to ours, so a pair of values would mean the bff's copy was kept.
func TestAccessRepliesCarryNoStore(t *testing.T) {
	check := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
			t.Errorf("Cache-Control = %q, want exactly [no-store]", got)
		}
		if got := rec.Header().Values("X-Content-Type-Options"); len(got) != 1 || got[0] != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want exactly [nosniff]", got)
		}
	}

	t.Run("a reply the bff did not mark", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)
		for _, pair := range accessPairs {
			rec := serveAccess(h, accessRequest(pair.method, pair.path, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d, want 200", pair.name, rec.Code)
			}
			check(t, rec)
		}
	})

	t.Run("a reply the bff marked the same way", func(t *testing.T) {
		up := newAccessUpstream(t)
		up.answer(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"state":"ended"}`)
		})
		h := accessHandler(t, accessSite(nil), up.server.URL)
		check(t, serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", "")))
	})

	t.Run("a reply the bff marked cacheable", func(t *testing.T) {
		up := newAccessUpstream(t)
		up.answer(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("X-Content-Type-Options", "")
			_, _ = io.WriteString(w, `{"state":"ok"}`)
		})
		h := accessHandler(t, accessSite(nil), up.server.URL)
		check(t, serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", "")))
	})

	t.Run("a refusal the edge made", func(t *testing.T) {
		up := newAccessUpstream(t)
		refusals := map[string]struct {
			site *Site
			req  func() *http.Request
			want int
		}{
			"no owner": {accessSite(func(s *Site) { s.OwnerUserID = "" }),
				func() *http.Request { return accessUnlock("") }, http.StatusNotFound},
			"foreign origin": {accessSite(nil), func() *http.Request {
				req := accessUnlock("")
				req.Header.Set("Origin", "https://evil.example")
				return req
			}, http.StatusForbidden},
			"not json": {accessSite(nil), func() *http.Request {
				req := accessUnlock("")
				req.Header.Set("Content-Type", "text/plain")
				return req
			}, http.StatusUnsupportedMediaType},
			"too large": {accessSite(nil), func() *http.Request {
				return accessRequest(http.MethodPost, "/_memql/access", strings.Repeat("a", 2<<10))
			}, http.StatusRequestEntityTooLarge},
		}
		for name, tc := range refusals {
			rec := serveAccess(accessHandler(t, tc.site, up.server.URL), tc.req())
			if rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d", name, rec.Code, tc.want)
			}
			t.Run(name, func(t *testing.T) { check(t, rec) })
		}

		// And the 429, which has to be earned.
		h := accessHandler(t, accessSite(nil), up.server.URL)
		var rec *httptest.ResponseRecorder
		for i := 0; i < 11; i++ {
			rec = serveAccess(h, accessUnlock(""))
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("the 11th unlock = %d, want 429", rec.Code)
		}
		t.Run("rate limited", func(t *testing.T) { check(t, rec) })
	})

	t.Run("a bff that cannot be reached", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		target := dead.URL
		dead.Close()
		h := accessHandler(t, accessSite(nil), target)

		rec := serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", ""))

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		check(t, rec)
	})
}

// THE EDGE DOES NOT EDIT THE BFF'S ANSWER. Status, body and the headers that
// carry meaning arrive as sent: the visitor's page branches on exactly these.
func TestAccessPassesTheBffsAnswerThrough(t *testing.T) {
	cases := []struct {
		name, method, path string
		status             int
		body               string
	}{
		{"opened", http.MethodPost, "/_memql/access", http.StatusOK, `{"gate":"g","subject":"s","expiresAt":"2030-01-01T00:00:00Z","remainingMs":1000}`},
		{"invalid", http.MethodPost, "/_memql/access", http.StatusUnauthorized, `{"state":"invalid"}`},
		{"expired", http.MethodPost, "/_memql/access", http.StatusForbidden, `{"state":"expired"}`},
		{"ended", http.MethodGet, "/_memql/access", http.StatusUnauthorized, `{"state":"ended"}`},
		{"left", http.MethodPost, "/_memql/access/leave", http.StatusNoContent, ``},
		{"unreachable construct", http.MethodPost, "/_memql/access/call", http.StatusNotFound, `{"error":"not_found"}`},
		{"bad arguments", http.MethodPost, "/_memql/access/call", http.StatusBadRequest, `{"error":"invalid_args"}`},
		{"refused", http.MethodPost, "/_memql/access/call", http.StatusBadGateway, `{"error":"refused"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newAccessUpstream(t)
			up.answer(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			h := accessHandler(t, accessSite(nil), up.server.URL)

			rec := serveAccess(h, accessRequest(tc.method, tc.path, ""))

			if rec.Code != tc.status || rec.Body.String() != tc.body {
				t.Fatalf("got %d %q, want the bff's %d %q", rec.Code, rec.Body.String(), tc.status, tc.body)
			}
			if got := rec.Header().Get("Content-Type"); tc.body != "" && got != "application/json; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want the bff's", got)
			}
		})
	}
}

// A SITE TAKEN OFFLINE WHILE A VISITOR IS INSIDE. The status switch in serve
// answers before this surface is reached, so a disabled site is the 503 and a
// draft or archived one the 404 every other path gets, with the visitor's
// cookie still in their browser and NOTHING forwarded for the bff to decide.
func TestAccessSiteOfflineAnswersBeforeForwarding(t *testing.T) {
	cases := []struct {
		status string
		want   int
	}{
		{"disabled", http.StatusServiceUnavailable},
		{"draft", http.StatusNotFound},
		{"archived", http.StatusNotFound},
		// A status this build does not know serves for nobody (epic memql#4794).
		{"retired-by-a-later-release", http.StatusNotFound},
	}
	for _, tc := range cases {
		for _, pair := range accessPairs {
			t.Run(tc.status+"/"+pair.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := accessHandler(t, accessSite(func(s *Site) { s.Status = tc.status }), up.server.URL)

				req := accessRequest(pair.method, pair.path, "")
				req.Header.Set("Cookie", "memql_access=mql_acs_a-visitor-already-inside")
				rec := serveAccess(h, req)

				if rec.Code != tc.want {
					t.Fatalf("a %s site = %d, want %d", tc.status, rec.Code, tc.want)
				}
				if up.count() != 0 {
					t.Fatalf("a %s site forwarded the request to the bff", tc.status)
				}
			})
		}
	}
}

// THE EDGE NEVER READS THE COOKIE'S VALUE AND NEVER DECIDES ACCESS. It forwards
// whatever the browser sent, including a value no session could have, and the
// bff alone says what it means; and the bff's Set-Cookie, the one thing that
// can open or end a session, reaches the browser exactly as written.
func TestAccessCookiePassesThrough(t *testing.T) {
	t.Run("the visitor's cookie reaches the bff", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		const cookie = "memql_access=mql_acs_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA; theme=dark"
		req := accessRequest(http.MethodGet, "/_memql/access", "")
		req.Header.Set("Cookie", cookie)
		serveAccess(h, req)

		if got := up.only(t).header.Get("Cookie"); got != cookie {
			t.Fatalf("the bff received Cookie %q, want %q", got, cookie)
		}
	})

	t.Run("a value no session could have is the bff's to refuse", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		req := accessRequest(http.MethodPost, "/_memql/access/call", `{"call":"x","args":{}}`)
		req.Header.Set("Cookie", "memql_access=not-even-a-token")
		rec := serveAccess(h, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want the bff's answer", rec.Code)
		}
		if got := up.only(t).header.Get("Cookie"); got != "memql_access=not-even-a-token" {
			t.Fatalf("the bff received Cookie %q: the edge judged the value", got)
		}
	})

	t.Run("no cookie at all is forwarded as none", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := accessHandler(t, accessSite(nil), up.server.URL)

		serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", ""))

		if got := up.only(t).header.Values("Cookie"); len(got) != 0 {
			t.Fatalf("the bff received Cookie %q for a request that had none", got)
		}
	})

	t.Run("Set-Cookie reaches the visitor as written", func(t *testing.T) {
		opening := &http.Cookie{
			Name: "memql_access", Value: "mql_acs_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
			Path: "/_memql/access", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		}
		clearing := &http.Cookie{
			Name: "memql_access", Value: "", Path: "/_memql/access", MaxAge: -1,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		}
		for name, cookie := range map[string]*http.Cookie{"opening": opening, "clearing": clearing} {
			t.Run(name, func(t *testing.T) {
				up := newAccessUpstream(t)
				up.answer(func(w http.ResponseWriter, _ *http.Request) {
					http.SetCookie(w, cookie)
					w.WriteHeader(http.StatusOK)
				})
				h := accessHandler(t, accessSite(nil), up.server.URL)

				rec := serveAccess(h, accessUnlock(""))

				got := rec.Header().Values("Set-Cookie")
				if len(got) != 1 || got[0] != cookie.String() {
					t.Fatalf("Set-Cookie = %q, want exactly [%s]", got, cookie.String())
				}
			})
		}
	})
}

// A DEPLOYMENT WITH NOWHERE TO FORWARD TO answers like a site with nothing
// here, rather than a 502 that names the cluster's plumbing to a stranger. The
// 404 is the same bytes as every other refusal of this kind.
func TestAccessWithNoUsableAPITargetIsRefused(t *testing.T) {
	for name, target := range map[string]string{
		"unset":     "",
		"blank":     "   ",
		"not a URL": "http://[::1",
		"no scheme": "://",
	} {
		t.Run(name, func(t *testing.T) {
			h := accessHandler(t, accessSite(nil), target)

			rec := serveAccess(h, accessRequest(http.MethodGet, "/_memql/access", ""))

			if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"not_found"}` {
				t.Fatalf("got %d %q, want the 404 not_found", rec.Code, rec.Body.String())
			}
		})
	}
}

// A REFUSAL THAT COSTS NOTHING TO MAKE MUST NOT COST THE VISITOR A TOKEN.
// Content type, origin and declared size are all read from the headers, and a
// page on another site can make a browser send a request that fails every one
// of them. If those refusals spent the address's budget, that page could lock a
// real visitor out of unlocking, and a whole office behind one address with
// them.
func TestAccessRefusalsBeforeTheLimiterSpendNothing(t *testing.T) {
	up := newAccessUpstream(t)
	h := accessHandler(t, accessSite(nil), up.server.URL)

	for i := 0; i < 30; i++ {
		notJSON := accessUnlock("203.0.113.9")
		notJSON.Header.Set("Content-Type", "text/plain")
		foreign := accessUnlock("203.0.113.9")
		foreign.Header.Set("Origin", "https://evil.example")
		oversize := accessRequest(http.MethodPost, "/_memql/access", strings.Repeat("a", 2<<10))
		oversize.Header.Set("X-Forwarded-For", "203.0.113.9")
		for _, req := range []*http.Request{notJSON, foreign, oversize} {
			if rec := serveAccess(h, req); rec.Code == http.StatusTooManyRequests || rec.Code == http.StatusOK {
				t.Fatalf("a structurally wrong request answered %d", rec.Code)
			}
		}
	}
	if up.count() != 0 {
		t.Fatalf("%d of the wrong requests reached the bff", up.count())
	}
	for i := 0; i < 10; i++ {
		if rec := serveAccess(h, accessUnlock("203.0.113.9")); rec.Code != http.StatusOK {
			t.Fatalf("unlock %d after the refusals = %d, want 200: they spent its budget", i+1, rec.Code)
		}
	}
	if rec := serveAccess(h, accessUnlock("203.0.113.9")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the 11th unlock = %d, want 429", rec.Code)
	}
}

// THE LIMITER ON ITS OWN, where a token costs no request. This is where the
// 600 a minute ceiling is asserted exactly, that session, leave and call draw
// on the one budget, that the key is (site, address), and that unlock draws on
// neither of the other two.
func TestAccessLimitsKeyAndRefill(t *testing.T) {
	// Each case starts from its own limiter with the (site1, address) budget for
	// everything but unlock spent to the last token, so no case depends on
	// another having run first.
	spent := func(t *testing.T) (*accessLimits, *accessClock) {
		t.Helper()
		clock := &accessClock{now: time.Unix(1_800_000_000, 0)}
		limits := newAccessLimits()
		limits.now = clock.Now
		ops := []string{accessOpSession, accessOpLeave, accessOpCall}
		for i := 0; i < 600; i++ {
			if ok, _ := limits.allow(ops[i%len(ops)], "site1", "203.0.113.9"); !ok {
				t.Fatalf("request %d was refused inside the 600", i+1)
			}
		}
		return limits, clock
	}

	t.Run("session, leave and call share 600 a minute per address", func(t *testing.T) {
		limits, _ := spent(t)
		for _, op := range []string{accessOpSession, accessOpLeave, accessOpCall} {
			ok, retry := limits.allow(op, "site1", "203.0.113.9")
			if ok {
				t.Fatalf("%s was allowed past the 600: it has a budget of its own", op)
			}
			if retry < 1 || retry > 60 {
				t.Fatalf("%s retry hint = %d, want whole seconds between 1 and 60", op, retry)
			}
		}
	})

	t.Run("the key is the site and the address", func(t *testing.T) {
		limits, _ := spent(t)
		if ok, _ := limits.allow(accessOpSession, "site1", "203.0.113.10"); !ok {
			t.Fatal("another address shared the spent budget")
		}
		if ok, _ := limits.allow(accessOpSession, "site2", "203.0.113.9"); !ok {
			t.Fatal("another site shared the spent budget")
		}
	})

	t.Run("unlock and the others draw on different budgets", func(t *testing.T) {
		limits, _ := spent(t)
		if ok, _ := limits.allow(accessOpUnlock, "site1", "203.0.113.9"); !ok {
			t.Fatal("unlock was refused because the other operations were spent")
		}

		fresh := newAccessLimits()
		for i := 0; i < 10; i++ {
			fresh.allow(accessOpUnlock, "site1", "203.0.113.9")
		}
		if ok, _ := fresh.allow(accessOpUnlock, "site1", "203.0.113.9"); ok {
			t.Fatal("the 11th unlock from one address was allowed")
		}
		if ok, _ := fresh.allow(accessOpSession, "site1", "203.0.113.9"); !ok {
			t.Fatal("a poll was refused because unlock was spent")
		}
	})

	t.Run("it refills continuously", func(t *testing.T) {
		limits, clock := spent(t)
		// 600 a minute is ten a second. 1.05 s is ten and a half, so exactly
		// ten pass and the count does not hang on a rounding at the boundary.
		clock.advance(1050 * time.Millisecond)
		passed := 0
		for i := 0; i < 20; i++ {
			if ok, _ := limits.allow(accessOpCall, "site1", "203.0.113.9"); ok {
				passed++
			}
		}
		if passed != 10 {
			t.Fatalf("%d came back after 1.05 s, want 10", passed)
		}
	})
}
