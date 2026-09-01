package memql

import (
	"os"
	"strings"
	"testing"
)

// The three custom-domain guards (epic memql#4805, design D10), pure half.
//
// Every case below runs with no database, no environment and no actor, which
// is what the split between validateCustomDomainHostname and its caller exists
// for: the rules that decide WHICH hostnames may be claimed are the ones an
// operator's typo runs into, and they should not need a cluster to exercise.

const testClusterDomain = "memql.example"

// GUARD 1. A hostname under this cluster's own domain is a DEPLOYABLE's
// hostname, owned by createSite and already routed by the one `*.<domain>`
// rule. Binding it here would mint a second Ingress and a second Certificate
// for a host the front door already serves, and two rules on one host makes
// which one answers a property of the controller's ordering.
func TestCustomDomainRefusesAHostnameUnderTheClustersOwnDomain(t *testing.T) {
	for _, host := range []string{
		"shop." + testClusterDomain,
		"deep.shop." + testClusterDomain,
		testClusterDomain, // the apex itself
		"SHOP." + strings.ToUpper(testClusterDomain),
	} {
		err := validateCustomDomainHostname(host, testClusterDomain)
		if err == nil {
			t.Errorf("%q was admitted as a custom domain; it is this cluster's own territory", host)
			continue
		}
		// The message has to point at the thing that DOES work, or an operator
		// reads a refusal and has nowhere to go.
		if !strings.Contains(err.Error(), "create the site with that name instead") {
			t.Errorf("refusal for %q does not name the alternative: %v", host, err)
		}
	}
}

// GUARD 2, shape half. A front-door host is served by a rule this cluster
// ships; binding one would request a certificate for the host the cluster's own
// sign-in surface answers on.
//
// DERIVED from frontdoor.Hosts rather than listed, so a new role reserves its
// hostname automatically -- the failure mode of a second copy is that adding a
// role silently opens its host to whoever asks for it first.
func TestCustomDomainRefusesEveryFrontDoorHost(t *testing.T) {
	for _, host := range []string{
		"api." + testClusterDomain,
		"identity." + testClusterDomain,
		"mcp." + testClusterDomain,
		"portal." + testClusterDomain,
		"os." + testClusterDomain,
	} {
		if err := validateCustomDomainHostname(host, testClusterDomain); err == nil {
			t.Errorf("front-door host %q was admitted as a custom domain", host)
		}
	}
}

// A client's own domain is exactly what this flow is for.
func TestCustomDomainAdmitsAClientsOwnDomain(t *testing.T) {
	for _, host := range []string{"www.acme.com", "acme.com", "shop.acme.co.uk", "a.b.c.example.org"} {
		if err := validateCustomDomainHostname(host, testClusterDomain); err != nil {
			t.Errorf("%q was refused: %v", host, err)
		}
	}
}

// A wildcard cannot be bound at all: ACME cannot issue one over HTTP-01, and a
// single wildcard dnsName fails the WHOLE order rather than just its own name
// (memql#4224). Refusing at the write is what stops a row existing that could
// only ever sit in `issuing`.
func TestCustomDomainRefusesAWildcard(t *testing.T) {
	err := validateCustomDomainHostname("*.acme.com", testClusterDomain)
	if err == nil {
		t.Fatal("a wildcard hostname was admitted")
	}
	if !strings.Contains(err.Error(), "HTTP-01") {
		t.Errorf("the refusal does not say why a wildcard cannot work: %v", err)
	}
}

func TestCustomDomainRefusesASingleLabel(t *testing.T) {
	if err := validateCustomDomainHostname("acme", testClusterDomain); err == nil {
		t.Fatal("a single label was admitted as a fully qualified domain")
	}
}

// An empty cluster domain would make the suffix test admit nothing at all,
// which is a fail-closed answer that reads to an operator as "binding a domain
// is broken". It is unreachable through customDomainPolicyDomain, which never
// returns empty -- but a caller passing "" must get a message that says which
// half is missing.
func TestCustomDomainRefusesWhenTheClustersOwnDomainDidNotResolve(t *testing.T) {
	err := validateCustomDomainHostname("www.acme.com", "")
	if err == nil {
		t.Fatal("a hostname was checked against no cluster domain at all")
	}
	if !strings.Contains(err.Error(), "did not resolve") {
		t.Errorf("the refusal does not name the missing half: %v", err)
	}
}

// GUARD 3's tunable. A zero or unparseable value falls back rather than meaning
// "unlimited": this is a rate-limit guard, so reading an operator's typo as "no
// limit" would remove exactly the protection the value exists to provide.
func TestCustomDomainMaxPerSiteFallsBackRatherThanMeaningUnlimited(t *testing.T) {
	for _, raw := range []string{"", "0", "-3", "lots", "  "} {
		t.Setenv(customDomainMaxPerSiteEnv, raw)
		if got := customDomainMaxPerSite(); got != defaultCustomDomainMaxPerSite {
			t.Errorf("%s=%q gave %d, want the default %d", customDomainMaxPerSiteEnv, raw, got, defaultCustomDomainMaxPerSite)
		}
	}
	t.Setenv(customDomainMaxPerSiteEnv, "12")
	if got := customDomainMaxPerSite(); got != 12 {
		t.Errorf("a valid override gave %d, want 12", got)
	}
}

// The cap's default is spelled in TWO places -- here and in
// integrations/customdomain's ConfigFromEnv -- because importing integrations
// from component/memql would invert the module direction. Two spellings of one
// number is a drift risk, so they are pinned together.
//
// It reads the CONSTANT out of the sibling source rather than importing it, for
// the reason above; a grep is a weaker instrument than a compiler, so the
// assertion names what it is reading and fails loudly if the shape moves.
func TestCustomDomainMaxPerSiteDefaultsAgree(t *testing.T) {
	const sibling = "../../integrations/customdomain/plugin.go"
	src, err := os.ReadFile(sibling)
	if err != nil {
		t.Skipf("cannot read %s: %v", sibling, err)
	}
	const marker = "defaultMaxPerSite = "
	i := strings.Index(string(src), marker)
	if i < 0 {
		t.Fatalf("%s no longer declares %q -- this gate has stopped measuring anything, "+
			"which is worse than failing. Re-point it at wherever the default moved.", sibling, marker)
	}
	rest := string(src[i+len(marker):])
	end := strings.IndexAny(rest, "\n\r")
	got := strings.TrimSpace(rest[:end])
	want := "5"
	if got != want || defaultCustomDomainMaxPerSite != 5 {
		t.Errorf("the per-site cap disagrees: %s says %s, this package says %d",
			sibling, got, defaultCustomDomainMaxPerSite)
	}
}

// A REMOVED BINDING DOES NOT WALK AGAIN. Its row survives as the record of what
// this cluster served and when; putting it back into `removing` would unbind
// objects that are already gone and write a second removal that removed
// nothing.
func TestCustomDomainTerminalStatus(t *testing.T) {
	if !customDomainTerminalStatus("removed") {
		t.Error("`removed` is not terminal")
	}
	for _, s := range []string{"pending_dns", "verifying", "issuing", "live", "removing", ""} {
		if customDomainTerminalStatus(s) {
			t.Errorf("%q was treated as terminal", s)
		}
	}
}
