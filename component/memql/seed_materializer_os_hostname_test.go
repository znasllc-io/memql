package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// applyOsSiteHostname is the memql#4705 hook: SeedMaterializer rewrites the
// OS site hostname on every global rematerialize from MEMQL_DOMAIN via
// frontdoor.OsHost. The committed DSL seed stays os.memql.localhost.

func TestApplyOsSiteHostname_FromMEMQLDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "os.memql.localhost", "siteId": "os"}
	applyOsSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.example.com" {
		t.Fatalf("hostname = %v, want os.example.com", got)
	}
}

func TestApplyOsSiteHostname_UnsetFallsBackToLocalhost(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	args := map[string]any{"hostname": "os.stale.example", "siteId": "os"}
	applyOsSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.memql.localhost" {
		t.Fatalf("hostname = %v, want os.memql.localhost when MEMQL_DOMAIN is unset", got)
	}
}

func TestApplyOsSiteHostname_RematerializeOverwritesStale(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "os.memql.localhost", "siteId": "os"}
	applyOsSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.example.com" {
		t.Fatalf("first rematerialize hostname = %v, want os.example.com", got)
	}
	applyOsSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.example.com" {
		t.Fatalf("second rematerialize hostname = %v, want os.example.com (overwrite, not skip)", got)
	}
}

func TestApplyOsSiteHostname_NoOpForOtherSeeds(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "shop.memql.localhost"}
	applyOsSiteHostname(&SeedDefinition{Name: "shop", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("non-os site seed must not be rewritten, got %v", got)
	}
	applyOsSiteHostname(&SeedDefinition{Name: "os", UseConcept: "agent"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("os-named non-site seed must not be rewritten, got %v", got)
	}
	// A site seed by another name is left alone. It said "portal" until epic
	// memql#4984 retired that seed, and the control is worth keeping under a
	// name that is not a platform site at all: the hook keys on the seed NAME,
	// so the mistake it guards against is a site whose name somebody adds
	// later, not one the repo happens to ship today.
	applyOsSiteHostname(&SeedDefinition{Name: "docs", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("a site seed the OS hook does not name must not be rewritten, got %v", got)
	}
}

func TestOsSiteHostname_AgreesWithTheFrontDoor(t *testing.T) {
	for _, domain := range []string{"example.com", "lab.example.com", "memql.localhost"} {
		if got, want := osSiteHostname(domain), frontdoor.OsHost(domain); got != want {
			t.Errorf("osSiteHostname(%q) = %q, frontdoor.OsHost = %q; the site row and the certificate disagree", domain, got, want)
		}
		var san bool
		for _, s := range frontdoor.CertificateSANs(domain) {
			if s == osSiteHostname(domain) {
				san = true
			}
		}
		if !san {
			t.Errorf("the OS site hostname %q is not a front-door certificate SAN (%v)", osSiteHostname(domain), frontdoor.CertificateSANs(domain))
		}
	}
	if got, want := osSiteHostname(""), frontdoor.OsHost("memql.localhost"); got != want {
		t.Errorf("fail-closed hostname = %q, want the derivation for the committed default %q", got, want)
	}
}

func TestOsSiteHostname_PureHelper(t *testing.T) {
	if got, want := osSiteHostname("example.com"), "os.example.com"; got != want {
		t.Fatalf("osSiteHostname(example.com) = %q, want %q", got, want)
	}
	if got, want := osSiteHostname(""), "os.memql.localhost"; got != want {
		t.Fatalf("osSiteHostname(\"\") = %q, want %q", got, want)
	}
	if got, want := osSiteHostname("   "), "os.memql.localhost"; got != want {
		t.Fatalf("osSiteHostname(whitespace) = %q, want %q", got, want)
	}
}
