package memql

import (
	"testing"
)

// applyPortalSiteHostname is the #4222 hook: SeedMaterializer rewrites the
// portal site hostname on every global rematerialize from MEMQL_DOMAIN via
// frontdoor.DomainDerivationSuffix. The committed DSL seed stays
// portal.memql.localhost; the materializer overwrites. These pin the three
// acceptance cases (set domain, unset fail-closed, rematerialize overwrite)
// without standing up an engine.

func TestApplyPortalSiteHostname_FromMEMQLDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "portal.memql.localhost", "siteId": "portal"}
	applyPortalSiteHostname(&SeedDefinition{Name: "portal", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "portal.example.com" {
		t.Fatalf("hostname = %v, want portal.example.com", got)
	}
}

func TestApplyPortalSiteHostname_UnsetFallsBackToLocalhost(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	args := map[string]any{"hostname": "portal.stale.example", "siteId": "portal"}
	applyPortalSiteHostname(&SeedDefinition{Name: "portal", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "portal.memql.localhost" {
		t.Fatalf("hostname = %v, want portal.memql.localhost when MEMQL_DOMAIN is unset", got)
	}
}

func TestApplyPortalSiteHostname_RematerializeOverwritesStale(t *testing.T) {
	// A leftover committed-default hostname must not survive a rematerialize
	// once the install domain is set -- that is why a kubectl patch of the
	// site row is out of scope (the next boot sweep would clobber it).
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "portal.memql.localhost", "siteId": "portal"}
	applyPortalSiteHostname(&SeedDefinition{Name: "portal", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "portal.example.com" {
		t.Fatalf("first rematerialize hostname = %v, want portal.example.com", got)
	}
	applyPortalSiteHostname(&SeedDefinition{Name: "portal", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "portal.example.com" {
		t.Fatalf("second rematerialize hostname = %v, want portal.example.com (overwrite, not skip)", got)
	}
}

func TestApplyPortalSiteHostname_NoOpForOtherSeeds(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "shop.memql.localhost"}
	applyPortalSiteHostname(&SeedDefinition{Name: "shop", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("non-portal site seed must not be rewritten, got %v", got)
	}
	applyPortalSiteHostname(&SeedDefinition{Name: "portal", UseConcept: "agent"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("portal-named non-site seed must not be rewritten, got %v", got)
	}
}

func TestPortalSiteHostname_PureHelper(t *testing.T) {
	if got, want := portalSiteHostname("example.com"), "portal.example.com"; got != want {
		t.Fatalf("portalSiteHostname(example.com) = %q, want %q", got, want)
	}
	if got, want := portalSiteHostname(""), "portal.memql.localhost"; got != want {
		t.Fatalf("portalSiteHostname(\"\") = %q, want %q", got, want)
	}
	if got, want := portalSiteHostname("   "), "portal.memql.localhost"; got != want {
		t.Fatalf("portalSiteHostname(whitespace) = %q, want %q", got, want)
	}
}
