package memql

import (
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// applyPlatformSiteHostname is the memql#4705 hook: SeedMaterializer rewrites
// every platform site's hostname on every global rematerialize from
// MEMQL_DOMAIN via frontdoor.PlatformSiteHost. The committed DSL seeds stay
// <name>.memql.localhost. It was the OS-only applyOsSiteHostname until
// memql#5518 seeded the VS Code landing page as the second platform site.

func TestApplyOsSiteHostname_FromMEMQLDomain(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "os.memql.localhost", "siteId": "os"}
	applyPlatformSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.example.com" {
		t.Fatalf("hostname = %v, want os.example.com", got)
	}
}

func TestApplyOsSiteHostname_UnsetFallsBackToLocalhost(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "")
	args := map[string]any{"hostname": "os.stale.example", "siteId": "os"}
	applyPlatformSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.memql.localhost" {
		t.Fatalf("hostname = %v, want os.memql.localhost when MEMQL_DOMAIN is unset", got)
	}
}

func TestApplyOsSiteHostname_RematerializeOverwritesStale(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "os.memql.localhost", "siteId": "os"}
	applyPlatformSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.example.com" {
		t.Fatalf("first rematerialize hostname = %v, want os.example.com", got)
	}
	applyPlatformSiteHostname(&SeedDefinition{Name: "os", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "os.example.com" {
		t.Fatalf("second rematerialize hostname = %v, want os.example.com (overwrite, not skip)", got)
	}
}

func TestApplyOsSiteHostname_NoOpForOtherSeeds(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "shop.memql.localhost"}
	applyPlatformSiteHostname(&SeedDefinition{Name: "shop", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("non-os site seed must not be rewritten, got %v", got)
	}
	applyPlatformSiteHostname(&SeedDefinition{Name: "os", UseConcept: "agent"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("os-named non-site seed must not be rewritten, got %v", got)
	}
	// A site seed by another name is left alone. It said "portal" until epic
	// memql#4984 retired that seed, and the control is worth keeping under a
	// name that is not a platform site at all: the hook keys on the seed NAME,
	// so the mistake it guards against is a site whose name somebody adds
	// later, not one the repo happens to ship today.
	applyPlatformSiteHostname(&SeedDefinition{Name: "docs", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "shop.memql.localhost" {
		t.Fatalf("a site seed the platform-site hook does not name must not be rewritten, got %v", got)
	}
}

// The VS Code landing page is the second platform site (memql#5518) and rides
// the same hook: rewritten from MEMQL_DOMAIN, fail-closed to the committed
// localhost default, and never mistaken for the OS.
func TestApplyPlatformSiteHostname_VSCodeSite(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args := map[string]any{"hostname": "vscode.memql.localhost", "siteId": "vscode"}
	applyPlatformSiteHostname(&SeedDefinition{Name: "vscode", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "vscode.example.com" {
		t.Fatalf("hostname = %v, want vscode.example.com", got)
	}

	t.Setenv("MEMQL_DOMAIN", "")
	args = map[string]any{"hostname": "vscode.stale.example", "siteId": "vscode"}
	applyPlatformSiteHostname(&SeedDefinition{Name: "vscode", UseConcept: "site"}, args)
	if got := args["hostname"]; got != "vscode.memql.localhost" {
		t.Fatalf("hostname = %v, want vscode.memql.localhost when MEMQL_DOMAIN is unset", got)
	}

	// The vscode NAME on a non-site concept is not a platform site.
	t.Setenv("MEMQL_DOMAIN", "example.com")
	args = map[string]any{"hostname": "untouched"}
	applyPlatformSiteHostname(&SeedDefinition{Name: "vscode", UseConcept: "agent"}, args)
	if got := args["hostname"]; got != "untouched" {
		t.Fatalf("vscode-named non-site seed must not be rewritten, got %v", got)
	}
}

// Every platform site's seeded hostname is a front-door certificate SAN, at
// any domain: the seed row and the certificate are one derivation.
func TestPlatformSiteHostname_EveryPlatformSiteIsACertificateSAN(t *testing.T) {
	for _, domain := range []string{"example.com", "lab.example.com", "memql.localhost", ""} {
		sans := frontdoor.CertificateSANs(domain)
		if domain == "" {
			sans = frontdoor.CertificateSANs(defaultSeedDomain)
		}
		for _, name := range frontdoor.PlatformSites() {
			host := platformSiteHostname(name, domain)
			if want := frontdoor.PlatformSiteHost(name, strings.TrimSpace(domain)); domain != "" && host != want {
				t.Errorf("platformSiteHostname(%q, %q) = %q, frontdoor.PlatformSiteHost = %q; the site row and the certificate disagree", name, domain, host, want)
			}
			if !slices.Contains(sans, host) {
				t.Errorf("platform site %q's hostname %q at domain %q is not a front-door certificate SAN (%v)", name, host, domain, sans)
			}
		}
	}
	if !isPlatformSiteSeed("vscode") || !isPlatformSiteSeed("os") {
		t.Error("os and vscode are the platform sites and must both be recognised by the hook")
	}
	if isPlatformSiteSeed("docs") || isPlatformSiteSeed("") {
		t.Error("a name outside frontdoor.PlatformSites() must not be treated as a platform site")
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
