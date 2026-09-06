package envregistry

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// TestOsOriginIsTheFrontDoorOsHost pins memql#4705 from this side: the OS
// shell's OAuth redirect and CORS origin are composed from frontdoor.OsHost,
// and its hostname is a front-door certificate SAN. Missing the CORS origin is
// a silent sign-in death (memql#3315).
//
// THE CLIENT ID IS `os`, AND IT WAS `portal` UNTIL EPIC memql#4984. The portal
// owned the client and the OS borrowed it -- one client, two redirect URIs --
// which is why this test used to assert the OPPOSITE of what it asserts now:
// that no second client had been minted, and that the portal callback had not
// been dropped when the OS one was added. Retiring the portal made the client
// a name for a thing that no longer exists, so the client was renamed and its
// portal redirect deleted rather than kept as an accept-either.
//
// Renaming it is safe because no client hardcodes it: a bundle reads its own
// client id out of the edge's runtime-config document, which resolves the
// request hostname against these registered redirect URIs
// (component/edge/runtimeconfig.go). The `portal` assertions below are the
// negative half -- neither the id nor the host may come back.
func TestOsOriginIsTheFrontDoorOsHost(t *testing.T) {
	for _, domain := range []string{"example.com", "lab.example.com", "memql.localhost"} {
		derived := DomainDerivations(domain)
		if len(derived) == 0 {
			t.Fatalf("DomainDerivations(%q) derived nothing; the assertions below would be vacuous", domain)
		}
		origin := "https://" + frontdoor.OsHost(domain)

		clients := derived["MEMQL_IDENTITY_REGISTERED_CLIENTS"]
		if !strings.Contains(clients, `"clientId":"os"`) {
			t.Errorf("domain %q: the OS client is missing: %s", domain, clients)
		}
		if strings.Contains(clients, `"clientId":"portal"`) {
			t.Errorf("domain %q: the retired portal OAuth client is still registered: %s", domain, clients)
		}
		if !strings.Contains(clients, origin+"/auth/callback") {
			t.Errorf("domain %q: the OS client is not registered at %s/auth/callback: %s", domain, origin, clients)
		}
		if strings.Contains(clients, "https://portal."+domain) {
			t.Errorf("domain %q: a portal redirect URI survives: %s", domain, clients)
		}

		var cors bool
		for _, o := range strings.Split(derived["MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS"], ",") {
			if o == origin {
				cors = true
			}
			if o == "https://portal."+domain {
				t.Errorf("domain %q: the retired portal origin is still a CORS origin: %q",
					domain, derived["MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS"])
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
