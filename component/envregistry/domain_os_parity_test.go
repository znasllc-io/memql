package envregistry

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// TestOsOriginIsTheFrontDoorOsHost pins memql#4705 from this side: the OS
// shell's extra OAuth redirect (on the existing portal client) and CORS
// origin are composed from frontdoor.OsHost. Missing the CORS origin is a
// silent sign-in death (memql#3315). No new client id.
func TestOsOriginIsTheFrontDoorOsHost(t *testing.T) {
	for _, domain := range []string{"example.com", "lab.example.com", "memql.localhost"} {
		derived := DomainDerivations(domain)
		if len(derived) == 0 {
			t.Fatalf("DomainDerivations(%q) derived nothing; the assertions below would be vacuous", domain)
		}
		origin := "https://" + frontdoor.OsHost(domain)
		portalOrigin := "https://" + frontdoor.PortalHost(domain)

		clients := derived["MEMQL_IDENTITY_REGISTERED_CLIENTS"]
		if strings.Contains(clients, `"clientId":"os"`) {
			t.Errorf("domain %q: a second OAuth client id was minted for the OS; it must reuse portal: %s", domain, clients)
		}
		if !strings.Contains(clients, `"clientId":"portal"`) {
			t.Errorf("domain %q: the portal client is missing: %s", domain, clients)
		}
		if !strings.Contains(clients, origin+"/auth/callback") {
			t.Errorf("domain %q: the portal client is not registered at %s/auth/callback: %s", domain, origin, clients)
		}
		if !strings.Contains(clients, portalOrigin+"/auth/callback") {
			t.Errorf("domain %q: the portal callback was dropped when the OS redirect was added: %s", domain, clients)
		}

		var cors bool
		for _, o := range strings.Split(derived["MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS"], ",") {
			if o == origin {
				cors = true
			}
		}
		if !cors {
			t.Errorf("domain %q: %s is not a CORS origin: %q", domain, origin, derived["MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS"])
		}

		var san bool
		for _, s := range frontdoor.CertificateSANs(domain) {
			if "https://"+s == origin {
				san = true
			}
		}
		if !san {
			t.Errorf("domain %q: the OS origin %s names a host that is not a front-door certificate SAN (%v)",
				domain, origin, frontdoor.CertificateSANs(domain))
		}
	}
}
