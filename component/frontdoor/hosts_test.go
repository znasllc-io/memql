package frontdoor

import (
	"strings"
	"testing"
)

const domain = "example.test"

// TestEveryHostStaysSingleLabel is the assertion the whole certificate story
// rests on.
//
// A TLS wildcard matches exactly ONE label. Every host this package composes is
// one label under the domain (the apex is zero, and is SAN'd explicitly), so
// `*.<domain>` covers all of them with one order and one renewal. A host that
// grew a second label would not be covered and would serve the controller's
// default certificate -- a browser name mismatch at a host nobody thinks is new.
func TestEveryHostStaysSingleLabel(t *testing.T) {
	for _, h := range Hosts(domain) {
		if h.Name == Apex(domain) {
			continue // zero labels by construction; SAN'd on its own
		}
		prefix := strings.TrimSuffix(h.Name, "."+domain)
		if prefix == h.Name {
			t.Fatalf("%q does not end in the cluster domain", h.Name)
		}
		if strings.Contains(prefix, ".") {
			t.Errorf("host %q has %d labels in front of the domain; a TLS wildcard covers exactly one, so this host is not covered by *.%s",
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

// TestHostsIsTheWholeSet pins the count, because the count is the property
// docs/public/operate/front-door.md is about: it is fixed by the closed role
// set and never grows with sites.
func TestHostsIsTheWholeSet(t *testing.T) {
	hosts := Hosts(domain)
	if len(hosts) != 5 {
		t.Errorf("Hosts returns %d rules, want 5 (three roles, the sites wildcard, the apex)", len(hosts))
	}

	seen := map[string]bool{}
	for _, h := range hosts {
		if seen[h.Name] {
			t.Errorf("%q appears twice; two Ingress rules for one host resolve by whichever the controller saw first", h.Name)
		}
		seen[h.Name] = true
	}
}

// TestCertificateSANsCoverEveryHost is the gate that makes the single-label
// property pay: every generated host must fall under a requested SAN, or it
// serves a certificate error at a host nobody thinks is new.
func TestCertificateSANsCoverEveryHost(t *testing.T) {
	sans := CertificateSANs(domain)
	for _, h := range Hosts(domain) {
		if !coveredBy(h.Name, sans) {
			t.Errorf("%q is served and the requested SANs are %v, none of which covers it", h.Name, sans)
		}
	}
}

// TestCertificateIsExactlyTwoSANs is the cost statement: one wildcard for every
// role host and every site, plus the apex, which no wildcard can cover.
func TestCertificateIsExactlyTwoSANs(t *testing.T) {
	sans := CertificateSANs(domain)
	if len(sans) != 2 {
		t.Fatalf("CertificateSANs returns %d entries (%v), want 2", len(sans), sans)
	}
	if sans[0] != "*."+domain {
		t.Errorf("first SAN is %q, want the wildcard that covers every single-label host", sans[0])
	}
	if sans[1] != domain {
		t.Errorf("second SAN is %q, want the apex -- *.%s matches one label and the bare domain has none", sans[1], domain)
	}
}

// coveredBy applies the ONE-LABEL wildcard rule rather than a substring match,
// because the one-label rule is the entire reason this package exists. A
// checker that accepted `*.example.test` for `api.staging.example.test` would
// pass the exact configuration the design refuses.
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

// TestTheSuffixHelperAgreesWithTheHostBuilders keeps genesis's derivation and
// the generator's rules from drifting: genesis composes many names at once by
// concatenating this suffix, and the generator calls the builders. Two spellings
// of the same rule would disagree as an issuer nothing is served at.
func TestTheSuffixHelperAgreesWithTheHostBuilders(t *testing.T) {
	if got, want := "api"+DomainDerivationSuffix(domain), RoleHost(RoleAPI, domain); got != want {
		t.Errorf("DomainDerivationSuffix composes %q, RoleHost gives %q", got, want)
	}
	if got, want := "portal"+DomainDerivationSuffix(domain), SiteHost("portal", domain); got != want {
		t.Errorf("DomainDerivationSuffix composes %q, SiteHost gives %q", got, want)
	}
}
