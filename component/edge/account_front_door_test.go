// component/edge/account_front_door_test.go -- resolution through an account's
// reserved front door (epic memql#5168, design D4/F).
package edge

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/frontdoor"
)

const (
	testReserved = "memql.acme.com"
	testAppHost  = "app." + testReserved
	testIDHost   = "id." + testReserved
)

// osSiteThroughDoor is what SiteForAccountFrontDoor returns: the OS site row,
// unchanged, with the account attached. NOT a site of its own -- one shell,
// one product.
func osSiteThroughDoor() *Site {
	return &Site{
		ID:          frontdoor.OsSite,
		Hostname:    "os.memql.localhost",
		Kind:        "spa",
		Status:      "live",
		APIProxy:    true,
		SystemOwned: true,
		Account:     &SiteAccount{ID: "acct-1", ReservedName: testReserved},
	}
}

func TestADoorResolvesToTheOsSiteWithItsAccount(t *testing.T) {
	ex := &stubExec{doors: map[string]*Site{testAppHost: osSiteThroughDoor()}}
	r := NewResolver(ex, time.Minute)

	site, err := r.Resolve(context.Background(), testAppHost)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if site == nil {
		t.Fatal("a live door resolved to nothing")
	}
	if site.ID != frontdoor.OsSite {
		t.Errorf("resolved site = %q, want the OS site %q -- a door is not a site of its own", site.ID, frontdoor.OsSite)
	}
	if site.Account == nil || site.Account.ReservedName != testReserved {
		t.Fatalf("site.Account = %+v, want the account behind the door", site.Account)
	}
}

// THE ORDER IS THE SAFETY. A deployable already answering on a name must never
// lose traffic to a door, and a client's OWN domain is a more specific claim
// than a name reserved under ours.
func TestResolutionOrderPutsTheDoorLast(t *testing.T) {
	own := &Site{ID: "site:own", Hostname: testAppHost, Status: "live"}
	alias := &Site{ID: "site:alias", Hostname: "shop.example.com", Status: "live"}

	t.Run("a site's own hostname wins", func(t *testing.T) {
		ex := &stubExec{
			rows:    map[string]*Site{testAppHost: own},
			aliases: map[string]*Site{testAppHost: alias},
			doors:   map[string]*Site{testAppHost: osSiteThroughDoor()},
		}
		site, _ := NewResolver(ex, time.Minute).Resolve(context.Background(), testAppHost)
		if site == nil || site.ID != "site:own" {
			t.Fatalf("resolved %v, want the site's own row", site)
		}
		if ex.aliasCalls != 0 || ex.doorCalls != 0 {
			t.Errorf("the later steps were asked (%d alias, %d door) after the first answered", ex.aliasCalls, ex.doorCalls)
		}
	})

	t.Run("a live custom domain beats a door", func(t *testing.T) {
		ex := &stubExec{
			aliases: map[string]*Site{testAppHost: alias},
			doors:   map[string]*Site{testAppHost: osSiteThroughDoor()},
		}
		site, _ := NewResolver(ex, time.Minute).Resolve(context.Background(), testAppHost)
		if site == nil || site.ID != "site:alias" {
			t.Fatalf("resolved %v, want the custom-domain binding", site)
		}
		if ex.doorCalls != 0 {
			t.Errorf("the door step was asked %d time(s) after the alias answered", ex.doorCalls)
		}
	})
}

// A miss at the third step is cached like the other two. Without it a scanner
// walking hostnames against the wildcard drives THREE queries per request
// instead of one -- the alias step's amplifier argument, one step further on.
func TestADoorMissIsCached(t *testing.T) {
	ex := &stubExec{}
	r := NewResolver(ex, time.Minute)
	for range 3 {
		if site, _ := r.Resolve(context.Background(), "app.unknown.example"); site != nil {
			t.Fatal("an unknown host resolved to something")
		}
	}
	if ex.doorCalls != 1 {
		t.Errorf("the door read ran %d times for 3 requests to the same unknown host; a miss must be cached", ex.doorCalls)
	}
}

// ===========================================================================
// The runtime-config document
// ===========================================================================

func envOf(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestTheRuntimeConfigCarriesTheDoorsAccountAndIdentityOrigin(t *testing.T) {
	env := envOf(map[string]string{
		"MEMQL_IDENTITY_BASE_URL": "https://identity.memql.localhost",
		"MEMQL_DOMAIN":            "memql.localhost",
	})
	doc := runtimeConfigForSite(context.Background(), osSiteThroughDoor(), env, true, nil)

	if doc.Account == nil {
		t.Fatal("the document carries no account block for a page served through a door")
	}
	if doc.Account.ReservedName != testReserved || doc.Account.ID != "acct-1" {
		t.Errorf("account = %+v, want {acct-1 %s}", doc.Account, testReserved)
	}
	if want := "https://" + testIDHost; doc.IdentityURL != want {
		t.Errorf("identityUrl = %q, want %q -- top-level /authorize navigation goes here, so the cluster's own host would take a client's employee off their own domain mid-flow", doc.IdentityURL, want)
	}
	if doc.IdentityAPIBaseURL != "" {
		t.Errorf("identityApiBaseUrl = %q, want empty: the four identity JSON paths are proxied same-origin, which is why there is no CORS story here", doc.IdentityAPIBaseURL)
	}
}

// A document served on the cluster's own host must be BYTE-IDENTICAL to what
// it was before doors existed -- the additive-only rule RuntimeConfig
// documents. Asserted on the encoded bytes rather than on the struct, because
// `omitempty` is what makes it true and a struct comparison would not see it.
func TestADocumentWithNoDoorIsUnchanged(t *testing.T) {
	env := envOf(map[string]string{
		"MEMQL_IDENTITY_BASE_URL": "https://identity.memql.localhost",
		"MEMQL_DOMAIN":            "memql.localhost",
	})
	plain := osSiteThroughDoor()
	plain.Account = nil

	raw, err := json.Marshal(runtimeConfigForSite(context.Background(), plain, env, true, nil))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if strings.Contains(string(raw), "account") {
		t.Errorf("the document mentions an account for a page served on the cluster's own host:\n%s", raw)
	}
	var doc RuntimeConfig
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if doc.IdentityURL != "https://identity.memql.localhost" {
		t.Errorf("identityUrl = %q, want the cluster's own", doc.IdentityURL)
	}
}

// ===========================================================================
// The CSP
// ===========================================================================

// The policy must name the origin the page was actually served at. Naming the
// OS site's cluster hostname instead is not merely untidy: wsOriginOf composes
// the WebSocket origin from it, so a door would advertise
// wss://os.<cluster-domain> while the page opens wss://app.<reservedName>.
func TestTheCspNamesTheDoorsOwnOrigins(t *testing.T) {
	env := envOf(map[string]string{"MEMQL_IDENTITY_BASE_URL": "https://identity.memql.localhost"})
	policy := policyForSite(httptest.NewRequest("GET", "/", nil), osSiteThroughDoor(), env, "")

	for _, want := range []string{
		"https://" + testAppHost,
		"wss://" + testAppHost,
		"https://" + testIDHost,
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("connect-src does not name %q:\n%s", want, policy)
		}
	}
	if strings.Contains(policy, "os.memql.localhost") {
		t.Errorf("the policy names the OS site's cluster hostname, which is not the origin this page was served at:\n%s", policy)
	}
	if strings.Contains(policy, "identity.memql.localhost") {
		t.Errorf("the policy names the cluster's identity origin, but this page's sign-in goes to the door's:\n%s", policy)
	}
}

// The two must agree, or sign-in fails in the way csp.go's header paragraph
// describes: the top-level /authorize navigation succeeds because connect-src
// does not govern navigation, and the callback fetch is then refused by a
// policy naming an origin nobody configured.
func TestTheCspNamesTheSameIdentityOriginTheRuntimeConfigDoes(t *testing.T) {
	env := envOf(map[string]string{
		"MEMQL_IDENTITY_BASE_URL": "https://identity.memql.localhost",
		"MEMQL_DOMAIN":            "memql.localhost",
	})
	for name, site := range map[string]*Site{
		"through a door":            osSiteThroughDoor(),
		"on the cluster's own host": {ID: frontdoor.OsSite, Hostname: "os.memql.localhost", Status: "live"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := runtimeConfigForSite(context.Background(), site, env, true, nil)
			policy := policyForSite(httptest.NewRequest("GET", "/", nil), site, env, "")
			if !strings.Contains(policy, doc.IdentityURL) {
				t.Errorf("runtime config sends the browser to %q; connect-src does not name it:\n%s", doc.IdentityURL, policy)
			}
		})
	}
}

// A door is the SAME OAuth client reached at a different origin -- one client,
// one registration, one consent record. The first version of
// runtimeConfigForSite resolved the client against the door's `app.` host,
// which is in no registered client's redirect list (that list is derived from
// MEMQL_DOMAIN), so the document came back with an empty oauthClientId and the
// shell had nothing to present. What widens for a door is the identity
// service's redirect-URI allowlist, not this lookup.
func TestADoorPresentsTheSameOauthClientAsTheClustersOwnHost(t *testing.T) {
	env := envOf(map[string]string{
		"MEMQL_IDENTITY_BASE_URL":           "https://identity.memql.localhost",
		"MEMQL_DOMAIN":                      "memql.localhost",
		"MEMQL_IDENTITY_REGISTERED_CLIENTS": `[{"clientId":"os","redirectUris":["https://os.memql.localhost/auth/callback"]}]`,
	})

	throughDoor := runtimeConfigForSite(context.Background(), osSiteThroughDoor(), env, true, nil)
	plain := osSiteThroughDoor()
	plain.Account = nil
	direct := runtimeConfigForSite(context.Background(), plain, env, true, nil)

	if throughDoor.OAuthClientID == "" {
		t.Fatal("a page served through a door has no OAuth client to present, so the shell cannot start sign-in at all")
	}
	if throughDoor.OAuthClientID != direct.OAuthClientID {
		t.Errorf("through a door the client is %q, on the cluster's own host %q -- a door is the same client at a different origin", throughDoor.OAuthClientID, direct.OAuthClientID)
	}
}
