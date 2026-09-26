package githubconnect

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
)

// manifest.go -- the GitHub App this cluster asks GitHub to create (design
// record 2026-09-20-github-app-setup, D3 and D5).
//
// GitHub's manifest flow registers an app from a JSON document instead of a
// form somebody fills in: a browser POSTs the manifest to github.com, the
// person confirms the app's name, and GitHub redirects back with a code that
// is exchanged ONCE for everything an operator used to copy out of a settings
// page -- the app id, the slug, the OAuth pair, the webhook secret and the
// private key.
//
// ===========================================================================
// THE MANIFEST IS COMPOSED HERE, FROM THE CLUSTER'S OWN DOMAIN, AND NOWHERE ELSE
// ===========================================================================
// It is never an argument and never built from a request. Two things follow
// from that, and both are the point:
//
//   - WHAT IS ASKED OF GITHUB CANNOT WIDEN. Contents read and metadata read is
//     the whole ask (decision C8 of the Connect design). A manifest a browser
//     could influence is a manifest that could ask for write access under this
//     cluster's name, on an authorization screen the owner was told to trust.
//   - THE CALLBACK CANNOT MOVE. The callback URL is where GitHub sends every
//     person's authorization code from then on; one composed from a request's
//     Host would be one an attacker chooses (the RedirectURI argument, again).
//
// It is exactly the registration docs/public/operate/github-connect.md walks
// an operator through by hand, so an app registered either way is the same app.

const (
	// AppSetupStartPath serves the one page whose job is to POST the manifest
	// to GitHub. It exists because the manifest flow BEGINS with a browser form
	// post to github.com, and both MemQL OS and the identity pages forbid
	// cross-origin form posts by policy (`form-action 'self'`). Widening the
	// edge's policy would widen it for every site the cluster hosts -- it is
	// deliberately site-agnostic -- so the post happens from one page, on the
	// identity service, under a policy written for that page alone.
	AppSetupStartPath = "/auth/github/app/new"

	// AppSetupCallbackPath is where GitHub sends the browser back with the
	// one-time code. The OAuth-callback class, like CallbackPath.
	AppSetupCallbackPath = "/auth/github/app/callback"

	// WebhookPath is where the app's one webhook posts: the inbound seam's
	// route for the source the packages pipeline listens on
	// (component/inbound.RoutePrefix + component/packages' default webhook
	// source). Spelled here because this module sits below both and cannot
	// import either; app/'s inbound wiring test pins the three together.
	WebhookPath = "/inbound/github"

	// PurposeAppSetup marks a state row as belonging to this flow, so it can
	// never be spent by the Connect callback, nor a connect state by this one.
	PurposeAppSetup = "app_setup"
	// PurposeConnect is the Connect flow's own purpose. Blank means the same:
	// every row written before the field existed is a connect state.
	PurposeConnect = "connect"
	// PurposeShopifyConnect is Connect Shopify's (design record
	// 2026-09-23-connect-shopify, 12.4): a person connecting a storefront's
	// Shopify store rides this row and this lock, and is neither of the above.
	PurposeShopifyConnect = "shopify_connect"
)

// Manifest is the document GitHub creates the app from. Field names are
// GitHub's.
type Manifest struct {
	Name           string          `json:"name"`
	URL            string          `json:"url"`
	Description    string          `json:"description"`
	HookAttributes ManifestWebhook `json:"hook_attributes"`
	RedirectURL    string          `json:"redirect_url"`
	CallbackURLs   []string        `json:"callback_urls"`
	// NO setup_url, deliberately. With "request user authorization during
	// installation" on, GitHub does not accept one -- "you will not be able to
	// enter a URL here. Users will instead be redirected to the Callback URL"
	// -- and the callback already serves the post-install landing
	// (github_callback.go). A manifest carrying both is one GitHub may refuse
	// on a page the owner cannot fix.
	RequestOAuthOnInstall bool              `json:"request_oauth_on_install"`
	Public                bool              `json:"public"`
	DefaultPermissions    map[string]string `json:"default_permissions"`
	// Omitted when empty: an app whose webhook is off subscribes to nothing.
	DefaultEvents []string `json:"default_events,omitempty"`
}

// ManifestWebhook is the app's one webhook.
type ManifestWebhook struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// RequestedPermissions is the whole ask (decision C8). Exported because the
// callback checks what GitHub says it created against it: the person can edit
// only the app's NAME on GitHub's confirmation page, so anything wider coming
// back is not the app this cluster asked for.
func RequestedPermissions() map[string]string {
	return map[string]string{"contents": "read", "metadata": "read"}
}

// maxAppNameLen is GitHub's limit on an app's name.
const maxAppNameLen = 34

// BuildManifest composes the app for one cluster.
//
// `domain` is the cluster's apex, e.g. "lab.example.com" -- the value every
// node derives its hosts from. An empty or malformed one is an error rather
// than a manifest full of blanks: GitHub would reject it on a page the owner
// cannot do anything about.
//
// `identityBaseURL` is this identity service's own base URL, and the callback
// is composed from IT rather than from the domain, through RedirectURI -- the
// same function, over the same value, that githubConnectBegin and the callback
// use for `redirect_uri`. GitHub matches an authorization's redirect_uri
// against the callback registered here, so the two must be one derivation: a
// callback rebuilt from the domain would agree on every cluster that is
// configured consistently and refuse every Connect on one that is not, with
// GitHub's "redirect_uri mismatch" as the only clue. Empty derives it from the
// domain, which is what a consistent cluster means by it anyway.
func BuildManifest(domain, identityBaseURL string) (Manifest, error) {
	domain = strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" || strings.ContainsAny(domain, "/:@? \t") {
		return Manifest{}, fmt.Errorf("the cluster domain %q is not a hostname, so the app's URLs cannot be composed", domain)
	}
	identityBase := strings.TrimRight(strings.TrimSpace(identityBaseURL), "/")
	if identityBase == "" {
		identityBase = "https://" + frontdoor.RoleHost(frontdoor.RoleIdentity, domain)
	}
	callback := RedirectURI(identityBase)
	public := DomainIsPubliclyReachable(domain)

	m := Manifest{
		Name:        appName(domain),
		URL:         "https://" + frontdoor.OsHost(domain),
		Description: "Lets the MemQL cluster at " + domain + " read the repositories you choose to deploy from.",
		RedirectURL: identityBase + AppSetupCallbackPath,
		// ONE CALLBACK, the route the Connect design approved. It is also where
		// GitHub lands somebody who has just INSTALLED the app, because
		// authorization is requested during installation.
		CallbackURLs:          []string{callback},
		RequestOAuthOnInstall: true,
		// INSTALLABLE ON ANY ACCOUNT. A private app can be installed only on
		// the account that owns it, which would make "install on another
		// organization" a link that works for nobody. Public here means
		// installable by somebody who has the link, not listed anywhere: an
		// installation on an account nobody on this cluster connected is an
		// installation nothing ever reads.
		Public:             true,
		DefaultPermissions: RequestedPermissions(),
		HookAttributes: ManifestWebhook{
			URL:    "https://" + frontdoor.RoleHost(frontdoor.RoleAPI, domain) + WebhookPath,
			Active: public,
		},
	}
	if public {
		// PUSHES, which is what lights the update cue, and nothing else.
		m.DefaultEvents = []string{"push"}
	} else {
		// GITHUB CANNOT REACH A LAPTOP, so on a name that can never resolve
		// publicly the webhook is registered OFF. Its URL still has to be one
		// GitHub's validator accepts, and whether it accepts a `.localhost` name
		// is not something this cluster can find out -- the only way to ask is
		// an owner's signed-in GitHub session, on a page where a refusal is
		// something they cannot fix. So the manifest does not depend on the
		// answer: the URL is one that is syntactically public and guaranteed
		// inert (RFC 2606 reserves example.com, and GitHub's own manifest
		// documentation uses it). Nothing is ever delivered to it, because with
		// the webhook off nothing is delivered at all; the ten-minute poll is
		// what notices a push on such a cluster, as the operator guide says.
		m.HookAttributes.URL = "https://example.com" + WebhookPath
	}
	return m, nil
}

// appName is what people read on GitHub's authorization screen. "MemQL on
// <domain>" says which cluster is asking, which matters to somebody who runs
// two. Truncated to GitHub's limit from the DOMAIN's end rather than refused:
// the owner can edit the name on GitHub's own page, and it is the one field
// there they can.
func appName(domain string) string {
	name := "MemQL on " + domain
	if len(name) > maxAppNameLen {
		name = strings.TrimRight(name[:maxAppNameLen], ".-")
	}
	return name
}

// JSON renders the manifest as the form field GitHub reads.
func (m Manifest) JSON() (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// organizationLogin is GitHub's own rule for an account login: alphanumerics
// and single hyphens, not starting or ending with one, at most 39 characters.
var organizationLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-(?:[A-Za-z0-9])){0,38}$`)

// ValidOrganization reports whether login could be a GitHub organization.
// Empty is valid and means "the person's own account".
func ValidOrganization(login string) bool {
	login = strings.TrimSpace(login)
	return login == "" || organizationLogin.MatchString(login)
}

// ManifestPostURL is where the browser posts the manifest: the person's own
// account, or an organization's. `oauthBase` is https://github.com in
// production and a test server in a test.
//
// The organization is path-escaped even though ValidOrganization admits nothing
// that needs it: this string becomes a form's action, and "validated earlier"
// is not a property a URL carries with it.
func ManifestPostURL(oauthBase, organization, state string) string {
	base := strings.TrimRight(strings.TrimSpace(oauthBase), "/")
	if base == "" {
		base = defaultOAuthBaseURL
	}
	path := "/settings/apps/new"
	if org := strings.TrimSpace(organization); org != "" {
		path = "/organizations/" + url.PathEscape(org) + path
	}
	return base + path + "?state=" + url.QueryEscape(state)
}

// reservedSuffixes are names that never resolve on the public internet: the
// special-use domains of RFC 6761 and RFC 6762, the documentation and test
// names of RFC 2606, and the private-use names people give a LAN.
var reservedSuffixes = []string{
	"localhost", "local", "test", "example", "invalid", "internal", "lan", "home.arpa", "localdomain",
}

// DomainIsPubliclyReachable reports whether GitHub could plausibly deliver a
// webhook to this cluster.
//
// A GUESS FROM THE NAME, and an honest one: it answers "is this name one that
// can never resolve publicly", not "is this cluster reachable". A real domain
// on a private network answers true, registers an active webhook, and GitHub's
// deliveries fail where the owner can see them -- while the poll goes on
// noticing pushes. That is the right side to be wrong on: the other mistake is
// a production cluster whose update cue arrives ten minutes late for ever,
// with nothing anywhere saying why.
func DomainIsPubliclyReachable(domain string) bool {
	host := strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	// A single label ("localhost", "cluster") is nobody's public name, and an
	// address is not a name at all.
	if !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return false
	}
	for _, suffix := range reservedSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return false
		}
	}
	return true
}
