// component/edge/resolve_test.go
package edge

import (
	"context"
	"testing"
	"time"
)

type stubExec struct {
	calls int
	rows  map[string]*Site
}

func (s *stubExec) SiteByHostname(_ context.Context, hostname string) (*Site, error) {
	s.calls++
	return s.rows[hostname], nil
}

func TestResolveFindsTheSiteForAHostname(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{
		"shop.example.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
	}}
	r := NewResolver(ex, time.Minute)

	got, err := r.Resolve(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == nil || got.ID != "site:1" {
		t.Fatalf("Resolve returned %+v, want site:1", got)
	}
}

// A miss is a nil site and no error -- an unknown hostname is a 404, not a
// server fault. Returning an error here would turn every scan of the front
// door into a page of 500s in the logs.
func TestResolveMissReturnsNilWithoutError(t *testing.T) {
	r := NewResolver(&stubExec{rows: map[string]*Site{}}, time.Minute)

	got, err := r.Resolve(context.Background(), "nope.example.com")
	if err != nil {
		t.Fatalf("Resolve returned an error for a miss: %v", err)
	}
	if got != nil {
		t.Fatalf("Resolve returned %+v for a miss, want nil", got)
	}
}

// The resolution is per-request on the hot path, so it caches. Without this
// every asset fetch on every page becomes a database round trip.
func TestResolveCachesWithinTheTTL(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{
		"shop.example.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
	}}
	r := NewResolver(ex, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), "shop.example.com"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if ex.calls != 1 {
		t.Errorf("executor called %d times, want 1 -- the resolver is not caching", ex.calls)
	}
}

// A miss caches too. Otherwise a scanner hitting random hostnames drives one
// query per request, which is a denial-of-service amplifier pointed at the
// database.
func TestResolveCachesMisses(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{}}
	r := NewResolver(ex, time.Minute)

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(context.Background(), "nope.example.com"); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if ex.calls != 1 {
		t.Errorf("executor called %d times for a miss, want 1", ex.calls)
	}
}

// The Host header carries a port on a non-default port, and browsers vary on
// case. Neither should produce a miss.
func TestResolveNormalizesTheHostHeader(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{
		"shop.example.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
	}}
	r := NewResolver(ex, time.Minute)

	for _, in := range []string{"shop.example.com:443", "Shop.Example.Com", "SHOP.EXAMPLE.COM:8443"} {
		got, err := r.Resolve(context.Background(), in)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", in, err)
		}
		if got == nil {
			t.Errorf("Resolve(%q) missed; the Host header was not normalized", in)
		}
	}
}
