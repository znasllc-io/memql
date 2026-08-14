package frontdoor

import (
	"strings"
	"testing"
)

const domain = "example.test"

// TestRoleHostsStaySingleLabel is the assertion the whole hyphenation choice
// exists for.
//
// A TLS wildcard matches exactly ONE label. If a labelled environment's role
// host were `api.staging.<domain>` it would not be covered by `*.<domain>`, and
// standing up an environment would mean issuing a certificate per environment
// per role. Hyphenating keeps every role host under the wildcard the cluster
// already has, forever, however many environments there are.
func TestRoleHostsStaySingleLabel(t *testing.T) {
	for _, label := range []string{"", "staging", "qa", "eu-west"} {
		for _, role := range Roles() {
			host := RoleHost(role, label, domain)
			prefix := strings.TrimSuffix(host, "."+domain)
			if prefix == host {
				t.Fatalf("%q does not end in the cluster domain", host)
			}
			if strings.Contains(prefix, ".") {
				t.Errorf("role host %q has %d labels in front of the domain; a TLS wildcard covers exactly one, so this host is not covered by *.%s",
					host, strings.Count(prefix, ".")+1, domain)
			}
		}
	}
}

func TestRoleHostsSpellTheProduct(t *testing.T) {
	for _, tc := range []struct{ role, label, want string }{
		{"api", "", "api.example.test"},
		{"identity", "", "identity.example.test"},
		{"mcp", "", "mcp.example.test"},
		{"api", "staging", "api-staging.example.test"},
		{"identity", "staging", "identity-staging.example.test"},
		{"mcp", "staging", "mcp-staging.example.test"},
	} {
		if got := RoleHost(Role(tc.role), tc.label, domain); got != tc.want {
			t.Errorf("RoleHost(%q, %q) = %q, want %q", tc.role, tc.label, got, tc.want)
		}
	}
}

// A site is inherently multi-label under a labelled environment: its own name
// occupies the label a role would hyphenate into.
func TestSiteHostsNestUnderTheLabel(t *testing.T) {
	if got, want := SiteHost("shop", "", domain), "shop.example.test"; got != want {
		t.Errorf("SiteHost unprefixed = %q, want %q", got, want)
	}
	if got, want := SiteHost("shop", "staging", domain), "shop.staging.example.test"; got != want {
		t.Errorf("SiteHost labelled = %q, want %q", got, want)
	}
	// The rule the design states in prose: a labelled site lives under the
	// CLUSTER's domain, never under the customer's.
	if strings.Contains(SiteHost("shop", "staging", domain), "acme") {
		t.Error("a labelled site host must be composed from the cluster domain alone")
	}
}

func TestSitesWildcardAndApex(t *testing.T) {
	if got, want := SitesWildcard("", domain), "*.example.test"; got != want {
		t.Errorf("SitesWildcard unprefixed = %q, want %q", got, want)
	}
	if got, want := SitesWildcard("staging", domain), "*.staging.example.test"; got != want {
		t.Errorf("SitesWildcard labelled = %q, want %q", got, want)
	}
	if got, want := Apex("", domain), domain; got != want {
		t.Errorf("Apex unprefixed = %q, want %q", got, want)
	}
	// Only ONE environment can own the bare domain, and it is the unprefixed
	// one. A second claimant would be two Ingress rules for one host in two
	// namespaces, which resolves by whichever the controller saw first.
	if got := Apex("staging", domain); got != "" {
		t.Errorf("Apex(labelled) = %q, want empty -- the bare domain belongs to the unprefixed environment", got)
	}
}

// TestHostsIsTheWholeProduct pins the counts, because the count is the property
// docs/public/operate/front-door.md is about: it grows with environments (a
// closed set the operator chooses) and never with sites.
func TestHostsIsTheWholeProduct(t *testing.T) {
	unprefixed := Hosts("", domain)
	if len(unprefixed) != 5 {
		t.Errorf("unprefixed environment has %d host rules, want 5 (three roles, the sites wildcard, the apex)", len(unprefixed))
	}
	labelled := Hosts("staging", domain)
	if len(labelled) != 4 {
		t.Errorf("labelled environment has %d host rules, want 4 (three roles, the sites wildcard -- no apex)", len(labelled))
	}

	seen := map[string]bool{}
	for _, h := range append(append([]Host{}, unprefixed...), labelled...) {
		if seen[h.Name] {
			t.Errorf("%q is claimed by two environments; two Ingress rules for one host resolve by whichever the controller saw first", h.Name)
		}
		seen[h.Name] = true
	}
}

// TestCertificateSANsCoverEveryHost is the gate that makes the hyphenation
// choice pay: every generated host must fall under a SAN this environment
// requests, or it serves a certificate error at a host nobody thinks is new.
func TestCertificateSANsCoverEveryHost(t *testing.T) {
	for _, label := range []string{"", "staging", "qa"} {
		sans := CertificateSANs(label, domain)
		for _, h := range Hosts(label, domain) {
			if !coveredBy(h.Name, sans) {
				t.Errorf("environment %q serves %q and requests SANs %v, none of which covers it",
					label, h.Name, sans)
			}
		}
	}
}

// TestLabelledEnvironmentsAddExactlyOneWildcard is the cost statement: standing
// up an environment costs ONE additional certificate SAN, not one per role and
// not one per site.
func TestLabelledEnvironmentsAddExactlyOneWildcard(t *testing.T) {
	base := CertificateSANs("", domain)   // *.<domain> + apex
	next := CertificateSANs("qa", domain) // *.<domain> + *.qa.<domain>

	if len(base) != 2 {
		t.Errorf("the unprefixed environment requests %d SANs, want 2 (the wildcard and the apex)", len(base))
	}
	if len(next) != 2 {
		t.Errorf("a labelled environment requests %d SANs, want 2 (the shared wildcard and its own sites wildcard)", len(next))
	}
	if next[0] != "*."+domain {
		t.Errorf("a labelled environment's first SAN is %q, want the shared *.%s that covers its single-label role hosts", next[0], domain)
	}
	if next[1] != "*.qa."+domain {
		t.Errorf("a labelled environment's second SAN is %q, want its own sites wildcard", next[1])
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
	if !coveredBy("api-staging.example.test", []string{"*.example.test"}) {
		t.Error("coveredBy rejected a single-label host under its wildcard")
	}
}

func TestValidateLabel(t *testing.T) {
	for _, ok := range []string{"", "staging", "qa", "eu-west", "e2"} {
		if err := ValidateLabel(ok); err != nil {
			t.Errorf("ValidateLabel(%q) = %v, want nil", ok, err)
		}
	}
	// A dotted label is the dangerous one: it silently turns a role host into
	// two labels, which the wildcard does not cover.
	for _, bad := range []string{"a.b", "Staging", "-lead", "trail-", "with space", "under_score", "*"} {
		if err := ValidateLabel(bad); err == nil {
			t.Errorf("ValidateLabel(%q) = nil, want an error", bad)
		}
	}
}

func TestSuffixHelpersAgreeWithTheHostBuilders(t *testing.T) {
	for _, label := range []string{"", "staging", "qa"} {
		if got, want := "api"+DomainDerivationSuffix(label, domain), RoleHost(RoleAPI, label, domain); got != want {
			t.Errorf("DomainDerivationSuffix(%q) composes %q, RoleHost gives %q", label, got, want)
		}
		if got, want := "portal"+SiteSuffix(label, domain), SiteHost("portal", label, domain); got != want {
			t.Errorf("SiteSuffix(%q) composes %q, SiteHost gives %q", label, got, want)
		}
	}
}
