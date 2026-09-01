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
	// aliases is the custom-domain half: hostname -> the site a LIVE binding
	// names. aliasCalls counts the second read so a test can prove it is not
	// reached when the first one answers.
	aliases    map[string]*Site
	aliasCalls int
}

func (s *stubExec) SiteByHostname(_ context.Context, hostname string) (*Site, error) {
	s.calls++
	return s.rows[hostname], nil
}

func (s *stubExec) SiteForCustomDomain(_ context.Context, hostname string) (*Site, error) {
	s.aliasCalls++
	return s.aliases[hostname], nil
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

// ---------------------------------------------------------------------------
// The custom-domain alias step (epic memql#4805, design D8)
// ---------------------------------------------------------------------------

// A client's own domain resolves to the deployable its LIVE binding names.
func TestResolveFollowsALiveCustomDomainToItsSite(t *testing.T) {
	ex := &stubExec{
		rows: map[string]*Site{},
		aliases: map[string]*Site{
			"www.acme.com": {ID: "site:1", Hostname: "shop.example.com", Status: "live"},
		},
	}
	r := NewResolver(ex, time.Minute)

	site, err := r.Resolve(context.Background(), "www.acme.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if site == nil {
		t.Fatal("a live custom domain resolved to no site -- the alias step did not run")
	}
	if site.ID != "site:1" {
		t.Errorf("site.ID = %q, want site:1 (the deployable the binding names)", site.ID)
	}
}

// THE ORDERING IS THE POINT. A site's own hostname always wins, so a binding
// can never take traffic away from a deployable already answering on that name
// -- and the second read is not even issued.
func TestResolvePrefersASitesOwnHostnameOverACustomDomain(t *testing.T) {
	ex := &stubExec{
		rows: map[string]*Site{
			"shop.example.com": {ID: "site:own", Hostname: "shop.example.com", Status: "live"},
		},
		aliases: map[string]*Site{
			"shop.example.com": {ID: "site:alias", Hostname: "shop.example.com", Status: "live"},
		},
	}
	r := NewResolver(ex, time.Minute)

	site, err := r.Resolve(context.Background(), "shop.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if site == nil || site.ID != "site:own" {
		t.Fatalf("site = %+v, want the deployable's own row (site:own)", site)
	}
	if ex.aliasCalls != 0 {
		t.Errorf("the custom-domain read ran %d time(s) for a hostname a site already answers on; "+
			"it must be asked only on a miss", ex.aliasCalls)
	}
}

// A hostname no site and no LIVE binding answers on is a miss, and the miss is
// cached -- otherwise the alias step would double a scanner's amplification
// rather than leaving it where it was.
func TestResolveCachesAMissAcrossBothReads(t *testing.T) {
	ex := &stubExec{rows: map[string]*Site{}, aliases: map[string]*Site{}}
	r := NewResolver(ex, time.Minute)

	for i := 0; i < 3; i++ {
		site, err := r.Resolve(context.Background(), "nobody.example.org")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if site != nil {
			t.Fatalf("resolved %+v for a hostname nothing serves", site)
		}
	}
	if ex.calls != 1 || ex.aliasCalls != 1 {
		t.Errorf("three requests drove %d site read(s) and %d custom-domain read(s), want 1 and 1 "+
			"-- a cached miss must cover both", ex.calls, ex.aliasCalls)
	}
}
