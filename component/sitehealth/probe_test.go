package sitehealth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebsiteResults(t *testing.T) {
	for _, tc := range []struct {
		code  int
		state string
	}{{200, "reachable"}, {204, "reachable"}, {302, "unknown"}, {401, "unknown"}, {403, "unknown"}, {404, "unavailable"}, {503, "unavailable"}} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			p := &Prober{Client: &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" || r.URL.String() != "https://shop.example.com/" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.UserAgent() != ProbeUserAgent {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader("homepage")), Header: make(http.Header)}, nil
			})}}
			o := p.Check(context.Background(), Site{ID: "shop", Hostname: "shop.example.com", BundleRef: "blob://one", Status: "live"})
			if o.State != tc.state || o.HTTPStatus != tc.code || o.CheckedAt.IsZero() {
				t.Fatalf("observation: %+v", o)
			}
		})
	}
}

func TestProbeTLSAndCrossOriginRedirect(t *testing.T) {
	// A real TLS server proves verification stays enabled. No insecure client.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "shop.example.com" {
			t.Errorf("Host lost: %s", r.Host)
		}
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
	}))
	defer server.Close()
	p, err := NewProber("example.com", server.Listener.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	o := p.Check(context.Background(), Site{Hostname: "shop.example.com"})
	if o.State != "unavailable" {
		t.Fatalf("untrusted TLS must not pass: %+v", o)
	}
	// Exercise redirect policy without letting the test contact that address.
	called := 0
	p.Client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		called++
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": []string{"https://elsewhere.example/"}}, Request: r}, nil
	})
	o = p.Check(context.Background(), Site{Hostname: "shop.example.com"})
	if called != 1 || o.State != "unknown" {
		t.Fatalf("cross-origin redirect followed or called healthy: calls=%d %+v", called, o)
	}
}

func TestInvalidTargetsAndMissingCheckerStayUnknown(t *testing.T) {
	var p *Prober
	for _, host := range []string{"", "foo@metadata", "foo/bar", "foo:8080", "foo?x=y", "shop.example.com"} {
		if o := p.Check(context.Background(), Site{Hostname: host}); o.State != "unknown" {
			t.Fatalf("%q: %+v", host, o)
		}
	}
}

func TestOnlyDuePublishedSitesAreProbed(t *testing.T) {
	now := time.Now()
	sites := []Site{{"fresh", "a", "one", "live"}, {"changed", "b", "new", "live"}, {"old", "c", "one", "live"}, {"draft", "d", "one", "draft"}, {"paused", "e", "one", "disabled"}, {"archived", "f", "one", "archived"}}
	old := map[string]Observation{"fresh": {Hostname: "a", BundleRef: "one", CheckedAt: now}, "changed": {Hostname: "b", BundleRef: "old", CheckedAt: now}, "old": {Hostname: "c", BundleRef: "one", CheckedAt: now.Add(-time.Hour)}}
	due := dueSites(sites, old, now)
	if len(due) != 2 || due[0].ID != "old" || due[1].ID != "changed" {
		t.Fatalf("due: %+v", due)
	}
}

func TestBrowserOrOwnerCannotRunSweep(t *testing.T) {
	i := &Integration{}
	for _, ctx := range []context.Context{context.Background(), auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})} {
		if _, err := i.sweep(ctx, nil, 0); err == nil {
			t.Fatal("untrusted sweep accepted")
		}
	}
}
