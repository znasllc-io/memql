package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
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

// TestPortalSiteHostname_AgreesWithTheFrontDoor pins the memql#4224 parity:
// the hostname this materializer writes onto the portal site row is the SAME
// derivation cmd/frontdoorhosts writes into the portal's Ingress rule and the
// front-door certificate's dnsNames, and envregistry writes into the portal's
// redirect URI. A second spelling here would be a certificate naming a host
// the site row does not carry -- the edge would 404 the name the certificate
// was issued for, or serve the portal at a name it was not.
func TestPortalSiteHostname_AgreesWithTheFrontDoor(t *testing.T) {
	for _, domain := range []string{"example.com", "lab.example.com", "memql.localhost"} {
		if got, want := portalSiteHostname(domain), frontdoor.PortalHost(domain); got != want {
			t.Errorf("portalSiteHostname(%q) = %q, frontdoor.PortalHost = %q; the site row and the certificate disagree", domain, got, want)
		}
		var san bool
		for _, s := range frontdoor.CertificateSANs(domain) {
			if s == portalSiteHostname(domain) {
				san = true
			}
		}
		if !san {
			t.Errorf("the portal site hostname %q is not a front-door certificate SAN (%v)", portalSiteHostname(domain), frontdoor.CertificateSANs(domain))
		}
	}
	// The fail-closed default is the committed seed's value, which is also
	// what the derivation yields for the committed default domain -- so an
	// unset MEMQL_DOMAIN and a MEMQL_DOMAIN of memql.localhost agree.
	if got, want := portalSiteHostname(""), frontdoor.PortalHost("memql.localhost"); got != want {
		t.Errorf("fail-closed hostname = %q, want the derivation for the committed default %q", got, want)
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
