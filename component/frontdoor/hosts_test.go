package frontdoor

import (
	"strings"
	"testing"
)

const domain = "example.test"

// TestEveryHostStaysSingleLabel is the assertion the sites wildcard RULE rests
// on.
//
// An Ingress wildcard host matches exactly ONE label, so `*.<domain>` routes
// `shop.<domain>` to the edge and would NOT route `shop.eu.<domain>`. Every
// host this package composes is one label under the domain (the apex is zero,
// and carries its own rule), so the one wildcard rule covers every site. A host
// that grew a second label would match no rule and answer with the controller's
// 404 at a name nobody thinks is new.
//
// It used to be the certificate story too -- one `*.<domain>` SAN covering
// every single-label host. memql#4224 retired that: the cloud issuer is
// HTTP-01, which cannot issue a wildcard, so the certificate names exact hosts
// and this property is about ROUTING only.
func TestEveryHostStaysSingleLabel(t *testing.T) {
	for _, h := range Hosts(domain) {
		if h.Name == Apex(domain) {
			continue // zero labels by construction; carries its own rule
		}
		prefix := strings.TrimSuffix(h.Name, "."+domain)
		if prefix == h.Name {
			t.Fatalf("%q does not end in the cluster domain", h.Name)
		}
		if strings.Contains(prefix, ".") {
			t.Errorf("host %q has %d labels in front of the domain; an Ingress wildcard matches exactly one, so this host is not routed by *.%s",
				h.Name, strings.Count(prefix, ".")+1, domain)
		}
	}
}

func TestRoleHostsSpellTheRoleSet(t *testing.T) {
	for _, tc := range []struct{ role, want string }{
		{"api", "api.example.test"},
		{"identity", "identity.example.test"},
		{"mcp", "mcp.example.test"},
	} {
		if got := RoleHost(Role(tc.role), domain); got != tc.want {
			t.Errorf("RoleHost(%q) = %q, want %q", tc.role, got, tc.want)
		}
	}
}

func TestSiteHostsAreOneLabelToo(t *testing.T) {
	if got, want := SiteHost("shop", domain), "shop.example.test"; got != want {
		t.Errorf("SiteHost = %q, want %q", got, want)
	}
	// A site host is composed from the CLUSTER's domain, never from a name the
	// site itself carries -- the operator must not need DNS they may not
	// control to stand a site up.
	if strings.Contains(SiteHost("shop", domain), "acme") {
		t.Error("a site host must be composed from the cluster domain alone")
	}
}

func TestSitesWildcardAndApex(t *testing.T) {
	if got, want := SitesWildcard(domain), "*.example.test"; got != want {
		t.Errorf("SitesWildcard = %q, want %q", got, want)
	}
	if got, want := Apex(domain), domain; got != want {
		t.Errorf("Apex = %q, want %q", got, want)
	}
}

// TestPortalHostIsASiteHost pins the ONE derivation of the portal's hostname
// (memql#4224). The engine seeds the portal site row from MEMQL_DOMAIN
// (component/memql's SeedMaterializer), envregistry derives the portal's
// redirect URI and CORS origin from the same value, and cmd/frontdoorhosts
// writes the portal's Ingress rule and certificate SAN. All three call
// PortalHost, so a disagreement -- a certificate naming a host the site row
// does not carry -- cannot be authored.
func TestPortalHostIsASiteHost(t *testing.T) {
	if got, want := PortalHost(domain), "portal.example.test"; got != want {
		t.Errorf("PortalHost = %q, want %q", got, want)
	}
	if got, want := PortalHost(domain), SiteHost(PortalSite, domain); got != want {
		t.Errorf("PortalHost = %q but SiteHost(PortalSite) = %q; the portal is site #1 and must be composed like any site", got, want)
	}
	if got, want := PortalHost(domain), PortalSite+DomainDerivationSuffix(domain); got != want {
		t.Errorf("PortalHost = %q but the suffix composition gives %q", got, want)
	}
}

// TestHostsIsTheWholeSet pins the count, because the count is the property
// docs/public/operate/front-door.md is about: it is fixed by the closed role
// set plus the platform's own site, and never grows with customer sites.
func TestHostsIsTheWholeSet(t *testing.T) {
	hosts := Hosts(domain)
	if len(hosts) != 6 {
		t.Errorf("Hosts returns %d rules, want 6 (three roles, the portal, the sites wildcard, the apex)", len(hosts))
	}

	seen := map[string]bool{}
	for _, h := range hosts {
		if seen[h.Name] {
			t.Errorf("%q appears twice; two Ingress rules for one host resolve by whichever the controller saw first", h.Name)
		}
		seen[h.Name] = true
	}

	var wildcards int
	for _, h := range hosts {
		if h.Wildcard {
			wildcards++
			if !strings.HasPrefix(h.Name, "*.") {
				t.Errorf("%q is flagged Wildcard but does not start with `*.`", h.Name)
			}
			if !h.Sites {
				t.Errorf("%q is the wildcard and must be a sites rule", h.Name)
			}
		} else if strings.HasPrefix(h.Name, "*") {
			t.Errorf("%q is a wildcard host that is not flagged Wildcard; the certificate derivation would request it", h.Name)
		}
	}
	if wildcards != 1 {
		t.Errorf("Hosts carries %d wildcard rules, want exactly 1 (the sites rule)", wildcards)
	}

	// The three rules that reach the edge: the portal (exact), every other
	// site (the wildcard), and the apex.
	var sites []string
	for _, h := range hosts {
		if h.Sites {
			sites = append(sites, h.Name)
		}
	}
	if want := strings.Join([]string{PortalHost(domain), SitesWildcard(domain), Apex(domain)}, ","); strings.Join(sites, ",") != want {
		t.Errorf("sites rules are %v, want [%s]", sites, want)
	}
}

// TestCertificateSANsAreExactlyTheExactHosts is the memql#4224 statement.
//
// The cloud issuer solves HTTP-01 only. ACME cannot serve an HTTP-01 challenge
// for `*.<domain>` -- there is no host to serve it at -- and a single wildcard
// dnsName fails the WHOLE order, so the Certificate sat Pending and every
// Ingress served the controller's self-signed default. The certificate
// therefore names every EXACT host the front door serves, and nothing else:
// no wildcard, and no host that is not an Ingress rule.
func TestCertificateSANsAreExactlyTheExactHosts(t *testing.T) {
	sans := CertificateSANs(domain)

	want := []string{
		RoleHost(RoleAPI, domain),
		RoleHost(RoleIdentity, domain),
		RoleHost(RoleMCP, domain),
		PortalHost(domain),
		Apex(domain),
	}
	if strings.Join(sans, ",") != strings.Join(want, ",") {
		t.Errorf("CertificateSANs = %v, want %v (every exact host, in rule order)", sans, want)
	}

	for _, s := range sans {
		if strings.HasPrefix(s, "*") {
			t.Errorf("CertificateSANs requests the wildcard %q; an HTTP-01 issuer fails the whole order on it (memql#4224)", s)
		}
	}
}

// TestEveryExactHostIsASANAndTheWildcardIsNot is the two-way gate between the
// host set and the SAN set. A served exact host with no SAN terminates TLS with
// the controller's default certificate and presents as a browser name mismatch
// at a host nobody thinks is new -- which is exactly how the portal failed on
// the first entry-shape cluster. A SAN with no rule is a name the order pays for
// and nothing serves.
func TestEveryExactHostIsASANAndTheWildcardIsNot(t *testing.T) {
	sans := CertificateSANs(domain)
	isSAN := map[string]bool{}
	for _, s := range sans {
		isSAN[s] = true
	}

	for _, h := range Hosts(domain) {
		if h.Wildcard {
			if isSAN[h.Name] {
				t.Errorf("the wildcard rule %q is requested as a SAN", h.Name)
			}
			continue
		}
		if !isSAN[h.Name] {
			t.Errorf("%q is served and is not a requested SAN (%v)", h.Name, sans)
		}
		delete(isSAN, h.Name)
	}
	for s := range isSAN {
		t.Errorf("SAN %q is requested but no host rule serves it", s)
	}
}

// TestCoveredByIsExactOnly: the SAN matcher the overlay gates use is the
// one-label wildcard rule, and it still has to be correct -- but with no
// wildcard SAN requested, every host has to match a SAN by EXACT name. This
// keeps the helper honest in both directions.
func TestCoveredByIsExactOnly(t *testing.T) {
	sans := CertificateSANs(domain)
	if coveredBy(SiteHost("shop", domain), sans) {
		t.Errorf("a customer site host is covered by the requested SANs %v; the certificate must not claim to cover hosts it does not name", sans)
	}
	if !coveredBy(PortalHost(domain), sans) {
		t.Errorf("the portal host is not covered by the requested SANs %v", sans)
	}
}

// coveredBy applies the ONE-LABEL wildcard rule rather than a substring match,
// so that a wildcard SAN, should one ever be requested again under a DNS-01
// issuer, is read the way TLS reads it. A checker that accepted
// `*.example.test` for `api.staging.example.test` would pass the exact
// configuration the design refuses.
func coveredBy(host string, sans []string) bool {
	for _, san := range sans {
		if san == host {
			return true
		}
		suffix, ok := strings.CutPrefix(san, "*")
		if !ok || !strings.HasSuffix(host, suffix) {
			continue
		}
		if labelPart := strings.TrimSuffix(host, suffix); labelPart != "" && !strings.Contains(labelPart, ".") {
			return true
		}
	}
	return false
}

func TestCoveredByRefusesAMultiLabelMatch(t *testing.T) {
	// The self-test on the checker above: if this passed, every assertion that
	// uses it would be vacuous.
	if coveredBy("api.staging.example.test", []string{"*.example.test"}) {
		t.Error("coveredBy accepted a two-label host under a one-label wildcard, so every SAN assertion in this file is vacuous")
	}
	if !coveredBy("api.example.test", []string{"*.example.test"}) {
		t.Error("coveredBy rejected a single-label host under its wildcard")
	}
}

// TestTheSuffixHelperAgreesWithTheHostBuilders keeps envregistry's derivation
// and the generator's rules from drifting: envregistry composes many names at
// once by concatenating this suffix, and the generator calls the builders. Two
// spellings of the same rule would disagree as an issuer nothing is served at.
func TestTheSuffixHelperAgreesWithTheHostBuilders(t *testing.T) {
	if got, want := "api"+DomainDerivationSuffix(domain), RoleHost(RoleAPI, domain); got != want {
		t.Errorf("DomainDerivationSuffix composes %q, RoleHost gives %q", got, want)
	}
	if got, want := "portal"+DomainDerivationSuffix(domain), PortalHost(domain); got != want {
		t.Errorf("DomainDerivationSuffix composes %q, PortalHost gives %q", got, want)
	}
}
