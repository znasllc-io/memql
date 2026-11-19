package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// calls_test.go -- THE EDGE HALF OF THE CALLS SURFACE (calls.go).
//
// Every test drives the real Handler against a fake bff and reuses access_test.go's
// fixtures (accessUpstream, accessRequest, the frozen clock), because the two surfaces
// stand on the same gates in the same order. A refusal is proved by the bff seeing
// nothing, and a forward by the bff seeing the stamp and the rewritten path -- never by a
// status code alone, because a 404 from the wrong branch looks like a 404 from the right
// one.

// callsPairs are the ten method and path pairs, where each lands on the bff, and the cap on
// its body. Spelled out rather than read off callsRoutes, so the table does not assert the
// routes against themselves.
var callsPairs = []struct {
	name, method, path, forwarded string
	limit                         int
}{
	{"unlock", http.MethodPost, "/_memql/calls/unlock", "/calls/unlock", 1 << 10},
	{"session", http.MethodGet, "/_memql/calls/session", "/calls/session", 1 << 10},
	{"start", http.MethodPost, "/_memql/calls/start", "/calls/start", 1 << 10},
	{"schedule", http.MethodPost, "/_memql/calls/schedule", "/calls/schedule", 4 << 10},
	{"reschedule", http.MethodPost, "/_memql/calls/reschedule", "/calls/reschedule", 4 << 10},
	{"cancel", http.MethodPost, "/_memql/calls/cancel", "/calls/cancel", 1 << 10},
	{"join", http.MethodPost, "/_memql/calls/join", "/calls/join", 1 << 10},
	{"queue", http.MethodGet, "/_memql/calls/queue", "/calls/queue", 1 << 10},
	{"host", http.MethodPost, "/_memql/calls/host", "/calls/host", 1 << 10},
	{"leave", http.MethodPost, "/_memql/calls/leave", "/calls/leave", 1 << 10},
}

// callsPair finds one of the ten by name, for the tests about one route's budget.
func callsPair(t *testing.T, name string) (method, path string) {
	t.Helper()
	for _, pair := range callsPairs {
		if pair.name == name {
			return pair.method, pair.path
		}
	}
	t.Fatalf("no calls pair named %q", name)
	return "", ""
}

// callsSite is a live, owned deployable with its calls switch ON and apiProxy OFF: the
// ordinary shape of a calls deployable, and the one that proves the surface does not
// borrow the proxy's switch.
func callsSite(mutate func(*Site)) *Site {
	return accessSite(func(s *Site) {
		s.Calls = true
		if mutate != nil {
			mutate(s)
		}
	})
}

func callsHandlerFor(t *testing.T, resolver Resolver, target string) (*Handler, *accessClock) {
	t.Helper()
	h := NewHandler(Options{
		Resolver:       resolver,
		Opener:         mapOpener(map[string]string{"index.html": "ROOT"}),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		APITarget:      target,
		IdentityTarget: target,
	})
	clock := &accessClock{now: time.Unix(1_800_000_000, 0)}
	h.callsRate.now = clock.Now
	return h, clock
}

func callsHandler(t *testing.T, site *Site, target string) *Handler {
	t.Helper()
	h, _ := callsHandlerFor(t, staticResolver{site: site}, target)
	return h
}

// callsFrom is a request on one of the pairs that arrived from addr: a single
// X-Forwarded-For entry, so it is the right-most as well as the only one.
func callsFrom(method, path, addr string) *http.Request {
	req := accessRequest(method, path, "")
	if addr != "" {
		req.Header.Add("X-Forwarded-For", addr)
	}
	return req
}

// callsRefusalBody is a refusal the edge made, in the calls protocol's words: the ones the
// bff answers with, so a page reads one vocabulary whichever of the two refused it.
type callsRefusalBody struct {
	Error        string `json:"error"`
	Reason       string `json:"reason"`
	RetryAfterMs int64  `json:"retryAfterMs"`
}

func callsRefusalOf(t *testing.T, rec *httptest.ResponseRecorder) callsRefusalBody {
	t.Helper()
	var body callsRefusalBody
	decoder := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("the reply is not a calls refusal: %q (%v)", rec.Body.String(), err)
	}
	return body
}

// requireCallsNotFound is the one 404: the bytes the bff gives an unknown route, a request
// with no stamp and a site whose switch is off.
func requireCallsNotFound(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"not_found"}` {
		t.Fatalf("got %d %q, want the one 404, byte for byte", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want JSON", got)
	}
}

// requireCallsStamp is the edge's stamp and nothing else: one site value and one owner
// value, the edge's own.
func requireCallsStamp(t *testing.T, hit accessHit) {
	t.Helper()
	if got := hit.header.Values(CallsStampSiteHeader); len(got) != 1 || got[0] != "site1" {
		t.Fatalf("stamped site = %q, want exactly [site1]", got)
	}
	if got := hit.header.Values(CallsStampOwnerHeader); len(got) != 1 || got[0] != "user-owner" {
		t.Fatalf("stamped owner = %q, want exactly [user-owner]", got)
	}
}

func requireNoCallsStamp(t *testing.T, hit accessHit) {
	t.Helper()
	if got := hit.header.Values(CallsStampSiteHeader); len(got) != 0 {
		t.Fatalf("a calls site stamp reached %s: %q", hit.path, got)
	}
	if got := hit.header.Values(CallsStampOwnerHeader); len(got) != 0 {
		t.Fatalf("a calls owner stamp reached %s: %q", hit.path, got)
	}
}

// THE ROUTE IS TEN PAIRS AND NOTHING ELSE. The rows that matter are the ones that look like
// it: the right path with the wrong method, the surface's own base, a trailing slash, a
// segment below a route, a route name nobody serves, a longer first segment, another case.
// Each must reach the generic proxy untouched -- with apiProxy on it is forwarded under the
// "/memql" root with no stamp, and with it off it is the 404 it always was -- which is what
// proves it did not enter this surface.
func TestCallsSurfaceAnswersTenPairsAndNoOthers(t *testing.T) {
	for _, pair := range callsPairs {
		t.Run("routed/"+pair.name, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(func(s *Site) { s.APIProxy = true }), up.server.URL)

			rec := serveAccess(h, accessRequest(pair.method, pair.path, ""))

			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d, want 200", pair.method, pair.path, rec.Code)
			}
			hit := up.only(t)
			if hit.method != pair.method || hit.path != pair.forwarded {
				t.Fatalf("the bff received %s %s, want %s %s", hit.method, hit.path, pair.method, pair.forwarded)
			}
			requireCallsStamp(t, hit)
		})
	}

	// want is what the generic proxy forwards a path as, spelled out rather than computed
	// with upstreamPath so the table does not assert the function against itself.
	notRouted := []struct{ name, method, path, want string }{
		{"GET on unlock", http.MethodGet, "/_memql/calls/unlock", "/memql/calls/unlock"},
		{"GET on start", http.MethodGet, "/_memql/calls/start", "/memql/calls/start"},
		{"GET on join", http.MethodGet, "/_memql/calls/join", "/memql/calls/join"},
		{"GET on leave", http.MethodGet, "/_memql/calls/leave", "/memql/calls/leave"},
		{"POST on session", http.MethodPost, "/_memql/calls/session", "/memql/calls/session"},
		{"POST on queue", http.MethodPost, "/_memql/calls/queue", "/memql/calls/queue"},
		{"DELETE on cancel", http.MethodDelete, "/_memql/calls/cancel", "/memql/calls/cancel"},
		{"PUT on host", http.MethodPut, "/_memql/calls/host", "/memql/calls/host"},
		{"PATCH on schedule", http.MethodPatch, "/_memql/calls/schedule", "/memql/calls/schedule"},
		{"HEAD on session", http.MethodHead, "/_memql/calls/session", "/memql/calls/session"},
		{"OPTIONS on join", http.MethodOptions, "/_memql/calls/join", "/memql/calls/join"},
		{"the surface's own base", http.MethodGet, "/_memql/calls", "/memql/calls"},
		{"a POST to the base", http.MethodPost, "/_memql/calls", "/memql/calls"},
		{"the base with its slash", http.MethodPost, "/_memql/calls/", "/memql/calls/"},
		{"a trailing slash on unlock", http.MethodPost, "/_memql/calls/unlock/", "/memql/calls/unlock/"},
		{"a trailing slash on session", http.MethodGet, "/_memql/calls/session/", "/memql/calls/session/"},
		{"a segment below join", http.MethodPost, "/_memql/calls/join/x", "/memql/calls/join/x"},
		{"a route nobody serves", http.MethodPost, "/_memql/calls/end", "/memql/calls/end"},
		{"a longer route name", http.MethodPost, "/_memql/calls/unlocked", "/memql/calls/unlocked"},
		{"a longer first segment", http.MethodPost, "/_memql/callsign", "/memql/callsign"},
		{"the route capitalised", http.MethodPost, "/_memql/calls/Unlock", "/memql/calls/Unlock"},
		{"the segment capitalised", http.MethodGet, "/_memql/Calls/session", "/memql/Calls/session"},
		{"a lower-case method", "post", "/_memql/calls/unlock", "/memql/calls/unlock"},
	}
	for _, tc := range notRouted {
		t.Run("not routed/"+tc.name+"/apiProxy on", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(func(s *Site) { s.APIProxy = true }), up.server.URL)

			serveAccess(h, accessRequest(tc.method, tc.path, ""))

			hit := up.only(t)
			if hit.path != tc.want {
				t.Fatalf("%s %s reached the bff as %q, want %q from the generic proxy", tc.method, tc.path, hit.path, tc.want)
			}
			requireNoCallsStamp(t, hit)
		})
		t.Run("not routed/"+tc.name+"/apiProxy off", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)

			rec := serveAccess(h, accessRequest(tc.method, tc.path, ""))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want the generic proxy's 404", tc.method, tc.path, rec.Code)
			}
			if up.count() != 0 {
				t.Fatalf("%s %s reached the bff", tc.method, tc.path)
			}
			if strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("%s %s got the calls surface's JSON refusal, so it entered that surface: %q",
					tc.method, tc.path, rec.Body.String())
			}
		})
	}

	// THE BFF'S OWN PATHS ARE NOT A WAY IN. Without the marker a path is the bundle's, so
	// /calls/unlock on a deployable's origin is whatever the deployable serves there, and the
	// bff never hears of it.
	for _, apiProxy := range []bool{false, true} {
		for _, target := range []struct{ method, path string }{
			{http.MethodPost, "/calls/unlock"}, {http.MethodGet, "/calls/session"}, {http.MethodPost, "/calls/join"},
		} {
			t.Run(fmt.Sprintf("the bff's own path is the bundle's/apiProxy=%v/%s %s", apiProxy, target.method, target.path), func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(func(s *Site) { s.APIProxy = apiProxy }), up.server.URL)

				serveAccess(h, accessRequest(target.method, target.path, ""))

				if up.count() != 0 {
					t.Fatalf("%s %s on the deployable's origin reached the bff", target.method, target.path)
				}
			})
		}
	}
}

// THE EDGE OWNS THE SWITCH. A deployable whose calls switch is off answers every one of the
// ten pairs with the one 404 and forwards nothing -- not to the surface's routes and not to
// the generic proxy either, whatever its apiProxy says -- so "calls are off" is decided
// before the hop and costs the bff nothing. The switch is the site's own and independent of
// apiProxy, and an off answer logs nothing, since anybody may ask.
func TestCallsSurfaceIsOffUnlessTheSiteSwitchIsOn(t *testing.T) {
	for _, apiProxy := range []bool{false, true} {
		for _, pair := range callsPairs {
			t.Run(fmt.Sprintf("off/apiProxy=%v/%s", apiProxy, pair.name), func(t *testing.T) {
				up := newAccessUpstream(t)
				var logs bytes.Buffer
				h := NewHandler(Options{
					Resolver:  staticResolver{site: accessSite(func(s *Site) { s.APIProxy = apiProxy })},
					Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
					Logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
					APITarget: up.server.URL,
				})

				rec := serveAccess(h, accessRequest(pair.method, pair.path, ""))

				requireCallsNotFound(t, rec)
				if up.count() != 0 {
					t.Fatalf("%s on a deployable with calls off reached the bff", pair.name)
				}
				if logs.Len() != 0 {
					t.Fatalf("an off switch logged, and anybody may ask: %q", logs.String())
				}
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Fatalf("Cache-Control = %q, want no-store on the refusal too", got)
				}
			})
			t.Run(fmt.Sprintf("on/apiProxy=%v/%s", apiProxy, pair.name), func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(func(s *Site) { s.APIProxy = apiProxy }), up.server.URL)

				rec := serveAccess(h, accessRequest(pair.method, pair.path, ""))

				if rec.Code != http.StatusOK {
					t.Fatalf("%s = %d, want 200 whatever apiProxy says", pair.name, rec.Code)
				}
				if hit := up.only(t); hit.path != pair.forwarded {
					t.Fatalf("the bff received %q, want %q", hit.path, pair.forwarded)
				}
			})
		}
	}

	t.Run("each deployable has its own switch", func(t *testing.T) {
		up := newAccessUpstream(t)
		h, _ := callsHandlerFor(t, accessHostResolver{
			"demo.example.com":  callsSite(nil),
			"other.example.com": accessSite(func(s *Site) { s.ID = "site2"; s.Hostname = "other.example.com" }),
		}, up.server.URL)

		on := serveAccess(h, accessRequest(http.MethodGet, "/_memql/calls/session", ""))
		off := accessRequest(http.MethodGet, "/_memql/calls/session", "")
		off.Host = "other.example.com"
		offRec := serveAccess(h, off)

		if on.Code != http.StatusOK {
			t.Fatalf("the deployable with calls on = %d, want 200", on.Code)
		}
		requireCallsNotFound(t, offRec)
		if up.count() != 1 {
			t.Fatalf("the bff received %d requests, want only the one from the deployable with calls on", up.count())
		}
	})

	// An isolated resource origin serves a bundle's assets and nothing else, so the copy the
	// resolver makes of its parent carries no calls surface, as it carries no API proxy.
	t.Run("an isolated resource origin has no calls surface", func(t *testing.T) {
		site := extensionSite()
		site.Calls = true
		resolver := NewResolver(&stubExec{rows: map[string]*Site{site.Hostname: site}}, time.Minute)

		got, err := resolver.Resolve(context.Background(), resourceHost)

		if err != nil || got == nil || got.ResourceParentHost != site.Hostname {
			t.Fatalf("the resource origin did not resolve to its parent: %+v %v", got, err)
		}
		if got.Calls {
			t.Fatal("the resource origin's copy kept the calls switch")
		}
		if parent, _ := resolver.Resolve(context.Background(), site.Hostname); parent == nil || !parent.Calls {
			t.Fatal("the parent lost its switch: the copy must not write through")
		}
	})

	// The switch is read off the row as every other switch is: absent and anything that is
	// not a true are off, because every row written before the field existed has no calls.
	t.Run("the switch is read off the row", func(t *testing.T) {
		cases := []struct {
			name string
			row  map[string]any
			want bool
		}{
			{"on", map[string]any{"id": "s", "calls": true}, true},
			{"off", map[string]any{"id": "s", "calls": false}, false},
			{"absent", map[string]any{"id": "s"}, false},
			{"a number", map[string]any{"id": "s", "calls": 1}, false},
			{"an object", map[string]any{"id": "s", "calls": map[string]any{}}, false},
		}
		for _, tc := range cases {
			if got := siteFromRow(tc.row).Calls; got != tc.want {
				t.Errorf("%s: Calls = %v, want %v", tc.name, got, tc.want)
			}
		}
	})
}

// WHAT THE EDGE ANSWERS ITSELF, ALL BEFORE THE HOP. A deployable with no owner has no
// authority for a call to run under, a deployment with nowhere to forward to has nothing
// here, and neither is told apart from a site with calls off: the one 404, logged nowhere,
// because a passer-by decides how many there are. A deployable that is not live is answered
// by the status switch before this surface is reached, as every other path is.
func TestCallsAnswersWhatHasNothingBehindItBeforeTheHop(t *testing.T) {
	for _, owner := range []string{"", "   "} {
		for _, pair := range callsPairs {
			t.Run(fmt.Sprintf("owner=%q/%s", owner, pair.name), func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(func(s *Site) { s.OwnerUserID = owner }), up.server.URL)

				requireCallsNotFound(t, serveAccess(h, accessRequest(pair.method, pair.path, "")))
				if up.count() != 0 {
					t.Fatal("a deployable with no owner reached the bff")
				}
			})
		}
	}

	for name, target := range map[string]string{"unset": "", "blank": "   ", "not a URL": "http://[::1", "no scheme": "://"} {
		t.Run("API target "+name, func(t *testing.T) {
			var logs bytes.Buffer
			h := NewHandler(Options{
				Resolver:  staticResolver{site: callsSite(nil)},
				Opener:    mapOpener(map[string]string{"index.html": "ROOT"}),
				Logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
				APITarget: target,
			})
			for _, pair := range callsPairs {
				requireCallsNotFound(t, serveAccess(h, accessRequest(pair.method, pair.path, "")))
			}
			// An unparseable target is the shared proxy builder's own error line, the case
			// access.go leaves to it too; a missing one says nothing a visitor can drive.
			if strings.TrimSpace(target) == "" && logs.Len() != 0 {
				t.Fatalf("ten requests with no API target logged %d bytes: %q", logs.Len(), logs.String())
			}
		})
	}

	for _, tc := range []struct {
		status string
		want   int
	}{
		{"disabled", http.StatusServiceUnavailable},
		{"draft", http.StatusNotFound},
		{"archived", http.StatusNotFound},
		{"retired-by-a-later-release", http.StatusNotFound},
	} {
		for _, pair := range callsPairs {
			t.Run(tc.status+"/"+pair.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(func(s *Site) { s.Status = tc.status }), up.server.URL)

				req := accessRequest(pair.method, pair.path, "")
				req.Header.Set("Cookie", memql.CallsCookieName+"=a-seat-already-in-a-call")
				rec := serveAccess(h, req)

				if rec.Code != tc.want {
					t.Fatalf("a %s deployable = %d, want %d", tc.status, rec.Code, tc.want)
				}
				if up.count() != 0 {
					t.Fatalf("a %s deployable forwarded the request", tc.status)
				}
			})
		}
	}
}

// A CLIENT-SUPPLIED STAMP DOES NOT SURVIVE, ON ANY PATH. The bff treats the two headers as a
// pointer it then verifies, but only because the edge is their sole author. Each forged
// header carries a SECOND value too, since a refactor from Set to Add would keep the first.
// On the ten pairs the edge's own values are what arrive; on everything else the edge
// forwards -- the generic proxy, the shopper surface, the access surface and the identity
// proxy -- none does, and the access surface's stamp is the access surface's alone.
func TestCallsStripsThenStamps(t *testing.T) {
	forge := func(req *http.Request) {
		req.Header.Set(CallsStampSiteHeader, "site-someone-elses")
		req.Header.Add(CallsStampSiteHeader, "site-another")
		req.Header.Set(CallsStampOwnerHeader, "user-someone-else")
		req.Header.Add(CallsStampOwnerHeader, "user-another")
		req.Header.Set(AccessStampSiteHeader, "site-someone-elses")
		req.Header.Set(AccessStampOwnerHeader, "user-someone-else")
	}

	for _, pair := range callsPairs {
		t.Run("stamped/"+pair.name, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)

			req := accessRequest(pair.method, pair.path, "")
			forge(req)
			serveAccess(h, req)

			hit := up.only(t)
			requireCallsStamp(t, hit)
			if got := hit.header.Values(AccessStampSiteHeader); len(got) != 0 {
				t.Fatalf("the access surface's site stamp reached a calls route: %q", got)
			}
			if got := hit.header.Values(AccessStampOwnerHeader); len(got) != 0 {
				t.Fatalf("the access surface's owner stamp reached a calls route: %q", got)
			}
		})
	}

	generic := []struct {
		name, method, path, want string
		site                     func(*Site)
		arrange                  func(*http.Request)
	}{
		{name: "the bridge", method: http.MethodGet, path: "/_memql/ws", want: "/memql/ws"},
		{name: "a route at the bff's own root", method: http.MethodPost, path: "/_memql/artifacts", want: "/artifacts"},
		{name: "the wrong method on a calls path", method: http.MethodGet, path: "/_memql/calls/unlock", want: "/memql/calls/unlock"},
		{name: "a segment under a calls route", method: http.MethodPost, path: "/_memql/calls/join/x", want: "/memql/calls/join/x"},
		{name: "a longer first segment", method: http.MethodPost, path: "/_memql/callsign", want: "/memql/callsign"},
		{name: "a shopper's form", method: http.MethodPost, path: "/_memql/forms/reviews/review", want: "/forms/reviews/review",
			site:    func(s *Site) { s.ShopperForms = true },
			arrange: func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }},
		{name: "the token endpoint", method: http.MethodPost, path: "/oauth/token", want: "/oauth/token"},
		{name: "a passkey step", method: http.MethodPost, path: "/auth/webauthn/login/begin", want: "/auth/webauthn/login/begin"},
	}
	for _, pair := range accessPairs {
		generic = append(generic, struct {
			name, method, path, want string
			site                     func(*Site)
			arrange                  func(*http.Request)
		}{name: "the access surface's " + pair.name, method: pair.method, path: pair.path, want: pair.forwarded})
	}
	for _, tc := range generic {
		t.Run("stripped/"+tc.name, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(func(s *Site) {
				s.APIProxy = true
				if tc.site != nil {
					tc.site(s)
				}
			}), up.server.URL)

			req := accessRequest(tc.method, tc.path, "")
			forge(req)
			if tc.arrange != nil {
				tc.arrange(req)
			}
			serveAccess(h, req)

			hit := up.only(t)
			if hit.path != tc.want {
				t.Fatalf("%s %s reached the upstream as %q, want %q: this is not the path under test", tc.method, tc.path, hit.path, tc.want)
			}
			requireNoCallsStamp(t, hit)
		})
	}

	// THE STAMP SURVIVES NO SPELLING OF ITS NAME. Only a real connection shows it: net/http
	// canonicalises a header's name as it reads the request, and httptest.NewRequest builds
	// the map by hand.
	spellings := func(name string) []string {
		return []string{strings.ToLower(name), strings.ToUpper(name), name, strings.ToLower(name[:1]) + strings.ToUpper(name[1:])}
	}
	raw := func(target string) string {
		var b strings.Builder
		b.WriteString("POST " + target + " HTTP/1.1\r\nHost: demo.example.com\r\n" +
			"Content-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n")
		for i, name := range []string{CallsStampSiteHeader, CallsStampOwnerHeader} {
			for j, spelling := range spellings(name) {
				fmt.Fprintf(&b, "%s: forged-%d-%d\r\n", spelling, i, j)
			}
		}
		b.WriteString("\r\n{}")
		return b.String()
	}
	send := func(t *testing.T, h *Handler, request string) string {
		t.Helper()
		front := httptest.NewServer(h)
		defer front.Close()
		conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(request)); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		reply, _ := io.ReadAll(conn)
		return string(reply)
	}
	t.Run("no spelling survives on the surface", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		if reply := send(t, h, raw("/_memql/calls/join")); !strings.HasPrefix(reply, "HTTP/1.1 200") {
			t.Fatalf("reply = %q, want a 200", strings.SplitN(reply, "\r\n", 2)[0])
		}
		requireCallsStamp(t, up.only(t))
	})
	t.Run("no spelling survives on the generic path", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(func(s *Site) { s.APIProxy = true }), up.server.URL)

		send(t, h, raw("/_memql/calls/join/x"))

		hit := up.only(t)
		if hit.path != "/memql/calls/join/x" {
			t.Fatalf("the request reached the bff as %q, want the generic proxy's path", hit.path)
		}
		requireNoCallsStamp(t, hit)
	})
}

// CROSS-SITE FORGERY: SameSite=Strict keeps the cookie off a cross-site request, JSON-only
// keeps a plain form from sending one, and this refuses the browser-made request that names
// another origin. Each refusal is the calls protocol's word for it and reaches nothing. A GET
// is not asked either question: it has no body, and the cookie a cross-site GET would need
// is one SameSite=Strict does not send.
func TestCallsRequiresJSONAndSameOrigin(t *testing.T) {
	for _, pair := range callsPairs {
		if pair.method != http.MethodPost {
			continue
		}
		for _, contentType := range []string{
			"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x",
			"application/jsonx", "application/vnd.api+json", "text/plain; type=application/json", "application/json, text/plain",
		} {
			t.Run(fmt.Sprintf("not json/%s/%q", pair.name, contentType), func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(nil), up.server.URL)

				req := accessRequest(pair.method, pair.path, `{}`)
				req.Header.Del("Content-Type")
				if contentType != "" {
					req.Header.Set("Content-Type", contentType)
				}
				rec := serveAccess(h, req)

				if rec.Code != http.StatusUnsupportedMediaType || callsRefusalOf(t, rec) != (callsRefusalBody{Error: "not_json"}) {
					t.Fatalf("Content-Type %q = %d %q, want 415 not_json", contentType, rec.Code, rec.Body.String())
				}
				if up.count() != 0 {
					t.Fatalf("Content-Type %q reached the bff", contentType)
				}
			})
		}
		for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON"} {
			t.Run(fmt.Sprintf("json/%s/%q", pair.name, contentType), func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(nil), up.server.URL)

				req := accessRequest(pair.method, pair.path, `{}`)
				req.Header.Set("Content-Type", contentType)
				if rec := serveAccess(h, req); rec.Code != http.StatusOK || up.count() != 1 {
					t.Fatalf("Content-Type %q = %d with %d upstream hits, want it forwarded", contentType, rec.Code, up.count())
				}
			})
		}

		for _, tc := range []struct{ name, host, origin string }{
			{"another site", "demo.example.com", "https://evil.example"},
			{"a suffix lookalike", "demo.example.com", "https://demo.example.com.evil.example"},
			{"a sibling deployable", "demo.example.com", "https://other.example.com"},
			{"the same host on another port", "demo.example.com", "https://demo.example.com:8443"},
			{"an opaque origin", "demo.example.com", "null"},
			{"something that is not an origin", "demo.example.com", "not a url"},
			{"an empty header", "demo.example.com", ""},
			{"the site's own host, asked for through a custom domain", "docs.customer.example", "https://demo.example.com"},
		} {
			t.Run("foreign origin/"+pair.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(nil), up.server.URL)

				req := accessRequest(pair.method, pair.path, `{}`)
				req.Host = tc.host
				req.Header.Set("Origin", tc.origin)
				rec := serveAccess(h, req)

				if rec.Code != http.StatusForbidden || callsRefusalOf(t, rec) != (callsRefusalBody{Error: "forbidden", Reason: "foreign_origin"}) {
					t.Fatalf("Origin %q on %q = %d %q, want 403 forbidden/foreign_origin", tc.origin, tc.host, rec.Code, rec.Body.String())
				}
				if up.count() != 0 {
					t.Fatalf("Origin %q reached the bff", tc.origin)
				}
			})
		}
		for _, tc := range []struct{ name, host, origin string }{
			{"no origin at all", "demo.example.com", "-"},
			{"its own origin", "demo.example.com", "https://demo.example.com"},
			{"its own origin in another case", "demo.example.com", "https://DEMO.example.com"},
			{"a custom domain under its own name", "docs.customer.example", "https://docs.customer.example"},
		} {
			t.Run("own origin/"+pair.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(nil), up.server.URL)

				req := accessRequest(pair.method, pair.path, `{}`)
				req.Host = tc.host
				if tc.origin != "-" {
					req.Header.Set("Origin", tc.origin)
				}
				if rec := serveAccess(h, req); rec.Code != http.StatusOK || up.count() != 1 {
					t.Fatalf("Origin %q on %q = %d with %d upstream hits, want it forwarded", tc.origin, tc.host, rec.Code, up.count())
				}
			})
		}
	}

	for _, name := range []string{"session", "queue"} {
		t.Run("a GET is not asked/"+name, func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)
			method, path := callsPair(t, name)

			req := accessRequest(method, path, "")
			req.Header.Del("Content-Type")
			if rec := serveAccess(h, req); rec.Code != http.StatusOK || up.count() != 1 {
				t.Fatalf("GET %s = %d with %d upstream hits, want it forwarded", path, rec.Code, up.count())
			}
		})
	}
}

// THE CAPS: 4 KiB for schedule and reschedule, which carry a booking's title, and 1 KiB for
// every other route, refused before the hop. The cap is on what is READ, not on what the
// request claims, so a body that declares no length is held to it as well; and a cap is only
// worth asserting if a body at exactly the limit still gets through whole.
func TestCallsCapsBodies(t *testing.T) {
	for _, pair := range callsPairs {
		t.Run(pair.name+"/exactly the cap is forwarded whole", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)
			body := strings.Repeat("a", pair.limit)

			if rec := serveAccess(h, accessRequest(pair.method, pair.path, body)); rec.Code != http.StatusOK {
				t.Fatalf("%d bytes = %d, want 200", pair.limit, rec.Code)
			}
			if got := up.only(t).body; got != body {
				t.Fatalf("the bff received %d bytes, want the %d it was sent", len(got), len(body))
			}
		})
		t.Run(pair.name+"/one byte over is refused before forwarding", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)

			rec := serveAccess(h, accessRequest(pair.method, pair.path, strings.Repeat("a", pair.limit+1)))

			if rec.Code != http.StatusRequestEntityTooLarge || callsRefusalOf(t, rec) != (callsRefusalBody{Error: "too_large"}) {
				t.Fatalf("%d bytes = %d %q, want 413 too_large", pair.limit+1, rec.Code, rec.Body.String())
			}
			if up.count() != 0 {
				t.Fatal("an oversize body reached the bff")
			}
		})
		t.Run(pair.name+"/a body with no declared length is capped while it is read", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)

			req := accessRequest(pair.method, pair.path, "")
			req.Body = io.NopCloser(strings.NewReader(strings.Repeat("a", pair.limit+1)))
			req.ContentLength = -1
			rec := serveAccess(h, req)

			if rec.Code != http.StatusRequestEntityTooLarge || callsRefusalOf(t, rec) != (callsRefusalBody{Error: "too_large"}) {
				t.Fatalf("an undeclared %d bytes = %d %q, want 413 too_large", pair.limit+1, rec.Code, rec.Body.String())
			}
			if up.count() != 0 {
				t.Fatal("an undeclared oversize body reached the bff")
			}
		})
		t.Run(pair.name+"/a body with no declared length within the cap is forwarded whole", func(t *testing.T) {
			up := newAccessUpstream(t)
			h := callsHandler(t, callsSite(nil), up.server.URL)
			body := strings.Repeat("a", pair.limit)

			req := accessRequest(pair.method, pair.path, "")
			req.Body = io.NopCloser(strings.NewReader(body))
			req.ContentLength = -1
			if rec := serveAccess(h, req); rec.Code != http.StatusOK {
				t.Fatalf("an undeclared %d bytes = %d, want 200", pair.limit, rec.Code)
			}
			if got := up.only(t).body; got != body {
				t.Fatalf("the bff received %d bytes, want the %d it was sent", len(got), len(body))
			}
		})
	}

	t.Run("a booking takes what an unlock refuses", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)
		body := strings.Repeat("a", 2<<10)

		unlock := serveAccess(h, accessRequest(http.MethodPost, "/_memql/calls/unlock", body))
		schedule := serveAccess(h, accessRequest(http.MethodPost, "/_memql/calls/schedule", body))
		reschedule := serveAccess(h, accessRequest(http.MethodPost, "/_memql/calls/reschedule", body))

		if unlock.Code != http.StatusRequestEntityTooLarge || schedule.Code != http.StatusOK || reschedule.Code != http.StatusOK {
			t.Fatalf("2 KiB: unlock = %d, schedule = %d, reschedule = %d, want 413, 200 and 200", unlock.Code, schedule.Code, reschedule.Code)
		}
	})

	// A CHUNKED UPLOAD REACHES THE BFF WITH A LENGTH: the edge read the body to cap it, so it
	// says how long it is. Over a real connection, which is the only thing that sets the
	// field a chunked request carries.
	t.Run("a chunked body reaches the bff with its length", func(t *testing.T) {
		up := newAccessUpstream(t)
		front := httptest.NewServer(callsHandler(t, callsSite(nil), up.server.URL))
		defer front.Close()
		body := strings.Repeat("a", 3000)

		req, err := http.NewRequest(http.MethodPost, front.URL+"/_memql/calls/schedule", io.NopCloser(strings.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()

		hit := up.only(t)
		if res.StatusCode != http.StatusOK || hit.body != body {
			t.Fatalf("status %d, the bff received %d bytes, want 200 and %d", res.StatusCode, len(hit.body), len(body))
		}
		if hit.contentLength != int64(len(body)) || len(hit.transferEncoding) != 0 {
			t.Fatalf("Content-Length %d with Transfer-Encoding %q, want %d and none", hit.contentLength, hit.transferEncoding, len(body))
		}
	})
}

// requireCallsLimited is the edge's 429: the bff's own words for a request made too soon
// (limited, rate), how long to wait in milliseconds, and the same wait in whole seconds.
func requireCallsLimited(t *testing.T, rec *httptest.ResponseRecorder, wantMs int64, wantSeconds string) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d %q, want 429", rec.Code, rec.Body.String())
	}
	if got := callsRefusalOf(t, rec); got != (callsRefusalBody{Error: "limited", Reason: "rate", RetryAfterMs: wantMs}) {
		t.Fatalf("refusal = %+v, want limited/rate with retryAfterMs %d", got, wantMs)
	}
	if got := rec.Header().Get("Retry-After"); got != wantSeconds {
		t.Fatalf("Retry-After = %q, want %q", got, wantSeconds)
	}
}

// THE BUDGETS, per replica: unlock 10 a minute per (site, address) and 60 per site; join 30
// and 600; a host's controls and the hang-up 120 per (site, address), shared by the two;
// every other route 600 per (site, address), shared. They bound load, not guessing: what
// stops guessing is the 128 bits of a key and the 160 of a link. Each operation's buckets
// are its own, so a crowd joining cannot spend what a key holder's unlock needs, a flood of
// unlocks cannot keep a guest out, a host's polling spends neither, and no polling or
// joining can keep a person from hanging up or a host from using the controls. The address
// is the right-most X-Forwarded-For entry, the ingress's own record.
func TestCallsRateLimitsUnlockAndJoinSeparately(t *testing.T) {
	unlock := func(addr string) *http.Request { return callsFrom(http.MethodPost, "/_memql/calls/unlock", addr) }
	join := func(addr string) *http.Request { return callsFrom(http.MethodPost, "/_memql/calls/join", addr) }
	poll := func(addr string) *http.Request { return callsFrom(http.MethodGet, "/_memql/calls/session", addr) }
	others := []string{"session", "start", "schedule", "reschedule", "cancel", "queue"}
	controls := []string{"host", "leave"}

	t.Run("unlock: ten per address", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 10; i++ {
			if rec := serveAccess(h, unlock("203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("unlock %d = %d, want 200 inside the budget", i+1, rec.Code)
			}
		}
		requireCallsLimited(t, serveAccess(h, unlock("203.0.113.9")), 6000, "6")
		if rec := serveAccess(h, unlock("203.0.113.10")); rec.Code != http.StatusOK {
			t.Fatalf("another address = %d, want its own budget", rec.Code)
		}
		if up.count() != 11 {
			t.Fatalf("the bff received %d unlocks, want 10 + 1", up.count())
		}
	})

	t.Run("unlock: rotating the leftmost entry buys no new budget", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 15; i++ {
			rec := serveAccess(h, unlock(fmt.Sprintf("10.66.0.%d, 203.0.113.9", i)))
			if want := i < 10; (rec.Code == http.StatusOK) != want {
				t.Fatalf("unlock %d with a fresh leftmost entry = %d: it is the same address", i+1, rec.Code)
			}
		}
	})

	t.Run("unlock: sixty per site, and one address cannot spend it", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		passed := 0
		for i := 0; i < 100; i++ {
			if serveAccess(h, unlock("203.0.113.9")).Code == http.StatusOK {
				passed++
			}
		}
		if passed != 10 {
			t.Fatalf("a flooding address got %d through, want its own 10", passed)
		}
		for i := 0; i < 50; i++ {
			if rec := serveAccess(h, unlock(fmt.Sprintf("198.18.1.%d", i))); rec.Code != http.StatusOK {
				t.Fatalf("visitor %d after the flood = %d, want 200: the flood spent the site's budget", i+1, rec.Code)
			}
		}
		requireCallsLimited(t, serveAccess(h, unlock("198.18.2.1")), 1000, "1")
		if rec := serveAccess(h, join("198.18.2.1")); rec.Code != http.StatusOK {
			t.Fatalf("a join after the site's unlock budget is gone = %d, want 200", rec.Code)
		}
		if rec := serveAccess(h, poll("198.18.2.1")); rec.Code != http.StatusOK {
			t.Fatalf("a poll after the site's unlock budget is gone = %d, want 200", rec.Code)
		}
	})

	t.Run("join: thirty per address, and unlock and the rest are whole", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 30; i++ {
			if rec := serveAccess(h, join("203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("join %d = %d, want 200 inside the budget", i+1, rec.Code)
			}
		}
		requireCallsLimited(t, serveAccess(h, join("203.0.113.9")), 2000, "2")
		if rec := serveAccess(h, join("203.0.113.10")); rec.Code != http.StatusOK {
			t.Fatalf("another address = %d, want its own budget", rec.Code)
		}
		if rec := serveAccess(h, unlock("203.0.113.9")); rec.Code != http.StatusOK {
			t.Fatalf("an unlock after the join budget was gone = %d, want 200", rec.Code)
		}
		for _, name := range append(append([]string(nil), others...), controls...) {
			method, path := callsPair(t, name)
			if rec := serveAccess(h, callsFrom(method, path, "203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("%s after the join budget was gone = %d, want 200", name, rec.Code)
			}
		}
	})

	t.Run("join: six hundred per site, spent by different addresses", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 600; i++ {
			addr := fmt.Sprintf("203.0.%d.%d", 113+i/30, i%30)
			if rec := serveAccess(h, join(addr)); rec.Code != http.StatusOK {
				t.Fatalf("join %d = %d, want 200 inside the site's 600", i+1, rec.Code)
			}
		}
		requireCallsLimited(t, serveAccess(h, join("198.18.0.1")), 100, "1")
		if rec := serveAccess(h, unlock("198.18.0.1")); rec.Code != http.StatusOK {
			t.Fatalf("an unlock after the site's join budget was gone = %d, want 200", rec.Code)
		}
	})

	t.Run("an exhausted unlock budget leaves joins alone", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 11; i++ {
			serveAccess(h, unlock("203.0.113.9"))
		}
		for i := 0; i < 30; i++ {
			if rec := serveAccess(h, join("203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("join %d after the unlocks = %d, want 200", i+1, rec.Code)
			}
		}
	})

	// THE REST SHARE SIX HUNDRED, at the real ceiling: the frozen clock makes it an exact
	// count rather than a count plus whatever refilled while the loop ran.
	t.Run("every other route shares six hundred per address", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 600; i++ {
			method, path := callsPair(t, others[i%len(others)])
			if rec := serveAccess(h, callsFrom(method, path, "10.66.0.1, 203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("request %d (%s) = %d, want 200 inside the 600", i+1, others[i%len(others)], rec.Code)
			}
		}
		for _, name := range others {
			method, path := callsPair(t, name)
			requireCallsLimited(t, serveAccess(h, callsFrom(method, path, "10.66.9.9, 203.0.113.9")), 100, "1")
		}
		if rec := serveAccess(h, poll("203.0.113.10")); rec.Code != http.StatusOK {
			t.Fatalf("a poll from another address = %d, want its own budget", rec.Code)
		}
		if rec := serveAccess(h, unlock("203.0.113.9")); rec.Code != http.StatusOK {
			t.Fatalf("an unlock after the polls = %d, want 200", rec.Code)
		}
		if rec := serveAccess(h, join("203.0.113.9")); rec.Code != http.StatusOK {
			t.Fatalf("a join after the polls = %d, want 200", rec.Code)
		}
		// THE POLLS CANNOT SPEND THE HANG-UP OR THE HOST'S CONTROLS (ruling final B-I1):
		// behind a fronting proxy everybody arriving through one front address shares that
		// address's budget, and a crowd's polls must not leave a person unable to leave.
		for _, name := range controls {
			method, path := callsPair(t, name)
			if rec := serveAccess(h, callsFrom(method, path, "203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("%s after the polls spent their budget = %d, want 200: the polls spent the hang-up's budget", name, rec.Code)
			}
		}
	})

	t.Run("the host's controls and the hang-up share a hundred and twenty per address", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 120; i++ {
			name := controls[i%len(controls)]
			method, path := callsPair(t, name)
			if rec := serveAccess(h, callsFrom(method, path, "203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("request %d (%s) = %d, want 200 inside the 120", i+1, name, rec.Code)
			}
		}
		for _, name := range controls {
			method, path := callsPair(t, name)
			requireCallsLimited(t, serveAccess(h, callsFrom(method, path, "203.0.113.9")), 500, "1")
		}
		method, path := callsPair(t, "leave")
		if rec := serveAccess(h, callsFrom(method, path, "203.0.113.10")); rec.Code != http.StatusOK {
			t.Fatalf("a hang-up from another address = %d, want its own budget", rec.Code)
		}
		for _, name := range others {
			method, path := callsPair(t, name)
			if rec := serveAccess(h, callsFrom(method, path, "203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("%s after the controls spent their budget = %d, want 200", name, rec.Code)
			}
		}
	})

	t.Run("each deployable has its own budgets", func(t *testing.T) {
		up := newAccessUpstream(t)
		h, _ := callsHandlerFor(t, accessHostResolver{
			"demo.example.com":  callsSite(nil),
			"other.example.com": callsSite(func(s *Site) { s.ID = "site2"; s.Hostname = "other.example.com" }),
		}, up.server.URL)

		for i := 0; i < 11; i++ {
			serveAccess(h, unlock("203.0.113.9"))
		}
		second := unlock("203.0.113.9")
		second.Host = "other.example.com"
		if rec := serveAccess(h, second); rec.Code != http.StatusOK {
			t.Fatalf("the same address on another deployable = %d, want that deployable's own budget", rec.Code)
		}
	})

	t.Run("the budget comes back", func(t *testing.T) {
		up := newAccessUpstream(t)
		h, clock := callsHandlerFor(t, staticResolver{site: callsSite(nil)}, up.server.URL)

		for i := 0; i < 31; i++ {
			serveAccess(h, join("203.0.113.9"))
		}
		// 30 a minute is one every two seconds; 2.5 s is a token and a quarter.
		clock.advance(2500 * time.Millisecond)
		if rec := serveAccess(h, join("203.0.113.9")); rec.Code != http.StatusOK {
			t.Fatalf("after 2.5 s = %d, want one join back", rec.Code)
		}
		if rec := serveAccess(h, join("203.0.113.9")); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("a second join after 2.5 s = %d, want only the one token", rec.Code)
		}
	})

	// A REFUSAL THAT COSTS NOTHING TO MAKE MUST NOT COST A TOKEN. A page on another site can
	// make a browser send a request that fails each of these, and if they spent the address's
	// budget that page could lock a real person out of unlocking or joining.
	t.Run("the refusals before the budgets spend nothing", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)

		for i := 0; i < 40; i++ {
			for _, name := range []string{"unlock", "join"} {
				method, path := callsPair(t, name)
				notJSON := callsFrom(method, path, "203.0.113.9")
				notJSON.Header.Set("Content-Type", "text/plain")
				foreign := callsFrom(method, path, "203.0.113.9")
				foreign.Header.Set("Origin", "https://evil.example")
				oversize := accessRequest(method, path, strings.Repeat("a", 2<<10))
				oversize.Header.Set("X-Forwarded-For", "203.0.113.9")
				for _, req := range []*http.Request{notJSON, foreign, oversize} {
					if rec := serveAccess(h, req); rec.Code == http.StatusTooManyRequests || rec.Code == http.StatusOK {
						t.Fatalf("a structurally wrong %s answered %d", name, rec.Code)
					}
				}
			}
		}
		if up.count() != 0 {
			t.Fatalf("%d of the wrong requests reached the bff", up.count())
		}
		for i := 0; i < 10; i++ {
			if rec := serveAccess(h, unlock("203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("unlock %d after the refusals = %d, want 200", i+1, rec.Code)
			}
		}
		for i := 0; i < 30; i++ {
			if rec := serveAccess(h, join("203.0.113.9")); rec.Code != http.StatusOK {
				t.Fatalf("join %d after the refusals = %d, want 200", i+1, rec.Code)
			}
		}
	})
}

// callsCookieCases are the Cookie headers a request can arrive with, and what each of the
// three kinds of way out hands its upstream: a calls route, an access route, and anything
// else.
var callsCookieCases = []struct {
	name      string
	lines     []string
	onCalls   []string
	onAccess  []string
	elsewhere []string
}{
	{name: "alone", lines: []string{"__Host-memql_calls=TOKEN"},
		onCalls: []string{"__Host-memql_calls=TOKEN"}},
	{name: "first", lines: []string{"__Host-memql_calls=TOKEN; memql_auth=A"},
		onCalls: []string{"__Host-memql_calls=TOKEN"}, elsewhere: []string{"memql_auth=A"}},
	{name: "last", lines: []string{"memql_auth=A; __Host-memql_calls=TOKEN"},
		onCalls: []string{"__Host-memql_calls=TOKEN"}, elsewhere: []string{"memql_auth=A"}},
	{name: "between", lines: []string{"memql_auth=A; __Host-memql_calls=TOKEN; theme=dark"},
		onCalls: []string{"__Host-memql_calls=TOKEN"}, elsewhere: []string{"memql_auth=A; theme=dark"}},
	{name: "beside the access cookie", lines: []string{"__Host-memql_access=ACCESS; __Host-memql_calls=TOKEN; theme=dark"},
		onCalls: []string{"__Host-memql_calls=TOKEN"}, onAccess: []string{"__Host-memql_access=ACCESS"}, elsewhere: []string{"theme=dark"}},
	{name: "each on a line of its own", lines: []string{"memql_auth=A", "__Host-memql_calls=TOKEN", "__Host-memql_access=ACCESS", "theme=dark"},
		onCalls: []string{"__Host-memql_calls=TOKEN"}, onAccess: []string{"__Host-memql_access=ACCESS"}, elsewhere: []string{"memql_auth=A", "theme=dark"}},
	{name: "twice, which only a client that is not a browser sends", lines: []string{"__Host-memql_calls=FIRST; theme=dark; __Host-memql_calls=SECOND"},
		onCalls: []string{"__Host-memql_calls=FIRST; __Host-memql_calls=SECOND"}, elsewhere: []string{"theme=dark"}},
	// THE REST OF THE HEADER IS LEFT AS IT WAS, spelling included: somebody else's
	// credential on its way to somebody else's service.
	{name: "the rest of the header keeps its spelling", lines: []string{"a=1;b=\"2 3\";  __Host-memql_calls=TOKEN;\tc=4"},
		onCalls: []string{"__Host-memql_calls=TOKEN"}, elsewhere: []string{"a=1;b=\"2 3\";\tc=4"}},
	// EXACT NAMES. Each of these is another cookie, to a browser as well.
	{name: "its neighbours are other cookies", lines: []string{"__host-memql_calls=x; __Host-memql_calls2=y; other=__Host-memql_calls=z; memql_calls=old"},
		elsewhere: []string{"__host-memql_calls=x; __Host-memql_calls2=y; other=__Host-memql_calls=z; memql_calls=old"}},
	{name: "no cookie at all"},
}

// THE CALLS COOKIE REACHES THE BFF ON THE TEN PAIRS, ALONE, AND NOWHERE ELSE. It is a __Host-
// cookie, so it has Path=/ and a browser sends it with every request to the deployable: on
// the ten pairs the bff gets it and no other credential a browser holds, and every other way
// out takes it from what it forwards and leaves the rest of the header alone. The access
// routes carry the access cookie and never this one.
func TestTheCallsCookieGoesNowhereElse(t *testing.T) {
	for _, pair := range callsPairs {
		for _, tc := range callsCookieCases {
			t.Run("on the surface/"+pair.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(nil), up.server.URL)

				req := accessCookieRequest(accessRequest(pair.method, pair.path, ""), tc.lines)
				req.Header.Set("Authorization", "Bearer a-token-the-bff-has-no-use-for")
				req.Header.Set("Sec-WebSocket-Protocol", "bearer, a-token-the-bff-has-no-use-for")
				serveAccess(h, req)

				hit := up.only(t)
				requireCookieLines(t, hit.header, tc.onCalls)
				if got := hit.header.Values("Authorization"); len(got) != 0 {
					t.Fatalf("Authorization reached the bff: %q", got)
				}
				if got := hit.header.Values("Sec-WebSocket-Protocol"); len(got) != 0 {
					t.Fatalf("Sec-WebSocket-Protocol, which carries a bearer too, reached the bff: %q", got)
				}
			})
		}
	}

	for _, pair := range accessPairs {
		for _, tc := range callsCookieCases {
			t.Run("on the access surface/"+pair.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(nil), up.server.URL)

				serveAccess(h, accessCookieRequest(accessRequest(pair.method, pair.path, ""), tc.lines))

				hit := up.only(t)
				if hit.path != pair.forwarded {
					t.Fatalf("reached the bff as %q, want %q", hit.path, pair.forwarded)
				}
				requireCookieLines(t, hit.header, tc.onAccess)
			})
		}
	}

	// Every other way out, and the calls surface's own lookalikes, which are the generic
	// proxy's.
	elsewhere := append([]struct {
		name         string
		method, path string
		site         func(*Site)
		arrange      func(*http.Request)
		wantPath     string
	}{
		{name: "the wrong method on a calls path", method: http.MethodGet, path: "/_memql/calls/join", site: func(s *Site) { s.APIProxy = true }, wantPath: "/memql/calls/join"},
		{name: "a segment under a calls route", method: http.MethodPost, path: "/_memql/calls/leave/x", site: func(s *Site) { s.APIProxy = true }, wantPath: "/memql/calls/leave/x"},
	}, accessOutbound...)
	for _, route := range elsewhere {
		for _, tc := range callsCookieCases {
			t.Run("elsewhere/"+route.name+"/"+tc.name, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(route.site), up.server.URL)

				req := accessCookieRequest(accessRequest(route.method, route.path, ""), tc.lines)
				if route.arrange != nil {
					route.arrange(req)
				}
				serveAccess(h, req)

				hit := up.only(t)
				if hit.path != route.wantPath {
					t.Fatalf("reached the upstream as %q, want %q: this is not the path under test", hit.path, route.wantPath)
				}
				requireCookieLines(t, hit.header, tc.elsewhere)
			})
		}
	}
}

// THE EDGE DOES NOT EDIT THE BFF'S ANSWER, BUT IT OWNS TWO HEADERS. Status, body, Set-Cookie
// and Retry-After arrive as the bff sent them -- the page branches on them, and Set-Cookie is
// the only thing that opens or ends a session -- while Cache-Control and X-Content-Type-
// Options are the edge's on every reply, refusals and a bff that cannot be reached included:
// no cache may keep a pass, a link or a place in line.
func TestCallsPassesTheBffsAnswerThroughUnderItsOwnHeaders(t *testing.T) {
	requireOwnHeaders := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
			t.Errorf("Cache-Control = %q, want exactly [no-store]", got)
		}
		if got := rec.Header().Values("X-Content-Type-Options"); len(got) != 1 || got[0] != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want exactly [nosniff]", got)
		}
	}

	t.Run("the answer as sent", func(t *testing.T) {
		seat := &http.Cookie{Name: memql.CallsCookieName, Value: "a-seat-session", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
		up := newAccessUpstream(t)
		up.answer(func(w http.ResponseWriter, _ *http.Request) {
			http.SetCookie(w, seat)
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Retry-After", "3")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"conflict","reason":"not_yet","retryAfterMs":2500}`)
		})
		h := callsHandler(t, callsSite(nil), up.server.URL)

		rec := serveAccess(h, accessRequest(http.MethodPost, "/_memql/calls/join", `{}`))

		if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"conflict","reason":"not_yet","retryAfterMs":2500}` {
			t.Fatalf("got %d %q, want the bff's answer", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Values("Set-Cookie"); len(got) != 1 || got[0] != seat.String() {
			t.Fatalf("Set-Cookie = %q, want exactly [%s]", got, seat.String())
		}
		if got := rec.Header().Get("Retry-After"); got != "3" {
			t.Fatalf("Retry-After = %q, want the bff's 3", got)
		}
		requireOwnHeaders(t, rec)
	})

	t.Run("a refusal the edge made", func(t *testing.T) {
		up := newAccessUpstream(t)
		h := callsHandler(t, callsSite(nil), up.server.URL)
		requireOwnHeaders(t, serveAccess(h, accessRequest(http.MethodPost, "/_memql/calls/unlock", strings.Repeat("a", 2<<10))))
	})

	t.Run("a bff that cannot be reached", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		target := dead.URL
		dead.Close()
		h := callsHandler(t, callsSite(nil), target)

		rec := serveAccess(h, accessRequest(http.MethodGet, "/_memql/calls/session", ""))

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		requireOwnHeaders(t, rec)
		// The one reply that is not JSON: the shared proxy builder's own 502, which calls.go
		// says a client must not read a body shape into.
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Errorf("Content-Type = %q, want the shared handler's text/plain; update calls.go if this has changed", got)
		}
	})
}

// A LINK OR A KEY PASTED INTO A URL TRAVELS NO FURTHER THAN IT HAS TO. The protocol carries
// every credential in a body or the cookie and has no query parameters, so a query never
// chooses the route and is dropped on all ten pairs: what reaches the bff is the canonical
// request target and nothing after it, so nothing that records a URL records the query.
func TestCallsAQueryStringTravelsNoFurther(t *testing.T) {
	for _, pair := range callsPairs {
		for _, query := range []string{"?token=SECRET", "?x=1&key=SECRET&y=2", "?/_memql/calls/host", "?"} {
			t.Run(pair.name+"/"+query, func(t *testing.T) {
				up := newAccessUpstream(t)
				h := callsHandler(t, callsSite(nil), up.server.URL)

				if rec := serveAccess(h, accessRequest(pair.method, pair.path+query, "")); rec.Code != http.StatusOK {
					t.Fatalf("%s %s%s = %d, want 200", pair.method, pair.path, query, rec.Code)
				}
				hit := up.only(t)
				if hit.path != pair.forwarded || hit.rawQuery != "" || hit.requestURI != pair.forwarded {
					t.Fatalf("the bff received %q (query %q), want exactly %q", hit.requestURI, hit.rawQuery, pair.forwarded)
				}
				requireCallsStamp(t, hit)
			})
		}
	}
}
