package envregistry

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// TestPortalOriginIsTheFrontDoorPortalHost pins the memql#4224 parity from
// this side: the portal's OAuth redirect URI and CORS origin are composed from
// frontdoor.PortalHost, the same call the engine's SeedMaterializer makes for
// the portal site row and cmd/frontdoorhosts makes for the portal's Ingress
// rule and certificate SAN.
//
// The failure a second spelling would produce is specific: identity matches
// redirect_uri by EXACT string, so a registered URI for a host the bundle is
// not served from is a 400 at /authorize with nothing in the portal's own
// logs -- and a certificate issued for that other host would look Ready.
func TestPortalOriginIsTheFrontDoorPortalHost(t *testing.T) {
	for _, domain := range []string{"example.com", "lab.example.com", "memql.localhost"} {
		derived := DomainDerivations(domain)
		if len(derived) == 0 {
			t.Fatalf("DomainDerivations(%q) derived nothing; the assertions below would be vacuous", domain)
		}
		origin := "https://" + frontdoor.PortalHost(domain)

		clients := derived["MEMQL_IDENTITY_REGISTERED_CLIENTS"]
		if !strings.Contains(clients, `{"clientId":"portal","redirectURIs":["`+origin+`/auth/callback"]}`) {
			t.Errorf("domain %q: the portal client is not registered at %s/auth/callback: %s", domain, origin, clients)
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

		// And the host the origin names is one the front-door certificate
		// carries, so the browser that is redirected there does not land on
		// the ingress controller's default certificate.
		var san bool
		for _, s := range frontdoor.CertificateSANs(domain) {
			if "https://"+s == origin {
				san = true
			}
		}
		if !san {
			t.Errorf("domain %q: the portal origin %s names a host that is not a front-door certificate SAN (%v)",
				domain, origin, frontdoor.CertificateSANs(domain))
		}
	}
}
