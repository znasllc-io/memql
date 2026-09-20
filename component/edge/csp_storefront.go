// component/edge/csp_storefront.go
package edge

import "strings"

// THE STOREFRONT ARM OF THE POLICY (memql#5534, design record G1).
//
// A kind="shopify_storefront" site served by the edge could not reach
// Shopify AT ALL before this file existed. policyForSite wrote one policy
// for every kind -- connect-src 'self' plus the site's own and the identity
// origins, img-src 'self' data: blob: -- so the browser's call to the
// Storefront API and every Shopify-hosted image were refused by the page's
// own policy before either reached the network. The kind was verified at the
// level of its model and its runtime document only, and as built it could
// not have worked.
//
// # Where the list comes from, and why that is the whole of the design
//
// Two sources, and the split is load-bearing:
//
//   - THE STORE-SPECIFIC HOST COMES FROM THE SITE'S RESOLVED BINDING --
//     site.Binding["storeDomain"], the same field runtimeconfig.go publishes
//     to the bundle. Never from a header, never from a query parameter, and
//     never from anything the BUNDLE supplies: a bundle that could name a
//     host in its own policy could name any host, which is the same as
//     having no policy.
//   - THE FIXED HOSTS ARE SHOPIFY'S OWN, identical for every store, and they
//     are taken from Shopify's own reference storefront rather than guessed.
//     Hydrogen's defaultDirectives (packages/hydrogen/src/csp/csp.ts) names
//     cdn.shopify.com, shopify.com and monorail-edge.shopifysvc.com, and
//     those three are exactly what is admitted here. The design record's
//     section 8 listed this set as UNVERIFIED; it is settled here, against
//     that page.
//
// # What is deliberately NOT here
//
//   - NO WILDCARD, on any directive. Not `*.shopify.com`, not `https:`. The
//     whole value of naming a store is lost the moment a pattern admits one
//     that was never bound.
//   - SCRIPT-SRC IS UNTOUCHED. Admitting a third party's script host is the
//     one widening that runs somebody else's code inside the storefront's
//     own origin, and nothing in the binding requires it: the Storefront API
//     is reached with fetch(), which connect-src governs. A storefront's
//     script-src is the same `'self'` plus its own inline hashes every other
//     kind gets (TestStorefrontScriptSrcIsNotWidened).
//   - NO frame-src. Checkout is Shopify's hosted checkout reached by
//     NAVIGATING to cart.checkoutUrl, and a navigation is governed by
//     neither frame-src nor connect-src. frame-ancestors stays 'none'.
//   - NO form-action widening. The storefront's own forms post to their own
//     origin; the Customer Account API's authorize step is a top-level
//     navigation, not a form post.
//
// # Kind is the gate, not the presence of a binding
//
// The same rule storefrontForSite states for the token: a storeDomain
// written onto an `spa` row -- by accident, or by someone probing -- widens
// nothing, because a site only takes this arm when it is DECLARED to be a
// storefront. Together with the byte-identity twins
// (TestNonStorefrontPoliciesAreUnchangedByTheStorefrontArm) that is what
// makes this change invisible to every site that is not one.
type storefrontCSP struct {
	connect []string
	img     []string
	media   []string
}

// shopifyFixedConnectHosts are the Shopify origins a storefront's JavaScript
// connects to that are the SAME for every store on the platform.
//
//   - cdn.shopify.com -- Shopify's asset CDN, which its own client SDKs also
//     fetch from. Named in Hydrogen's default connectSrc.
//   - shopify.com -- where the Customer Account API's OAuth authorization,
//     token, logout and GraphQL endpoints resolve. The authorize step is a
//     navigation and needs no source here; the TOKEN EXCHANGE is a fetch()
//     and does, and without it sign-in appears to proceed and then fails
//     silently at the callback -- the exact failure csp.go's header
//     paragraph describes for the identity service, one vendor over.
//   - monorail-edge.shopifysvc.com -- Shopify's analytics beacon. Named in
//     Hydrogen's default connectSrc, so a storefront built the way Shopify
//     documents reports its own analytics rather than emitting a violation
//     naming a host nobody configured.
var shopifyFixedConnectHosts = []string{
	"https://cdn.shopify.com",
	"https://shopify.com",
	"https://monorail-edge.shopifysvc.com",
}

// shopifyFixedAssetHosts are where Shopify serves product images, collection
// images and hosted video. One host today, kept as a list because it is the
// shape that will take a second without touching a caller.
var shopifyFixedAssetHosts = []string{
	"https://cdn.shopify.com",
}

// storefrontSources is the storefront arm's whole contribution, or the zero
// value for every other site. The zero value adds nothing anywhere, which is
// what keeps the spa and static policies byte-identical.
func storefrontSources(site *Site) storefrontCSP {
	if site == nil || site.Kind != storefrontKind {
		return storefrontCSP{}
	}

	out := storefrontCSP{
		connect: append([]string(nil), shopifyFixedConnectHosts...),
		img:     append([]string(nil), shopifyFixedAssetHosts...),
		media:   append([]string(nil), shopifyFixedAssetHosts...),
	}

	// THE BOUND STORE, VALIDATED BEFORE IT CAN LAND IN A RESPONSE HEADER.
	// This is row data, not literal code, and it takes the same validHost
	// whitelist the site's own hostname takes -- for csp.go's own stated
	// reason: a malformed value must DROP the source rather than inject
	// something unparseable, or worse a second header, into the policy. An
	// unbound storefront is left with Shopify's fixed hosts and no invented
	// one, which is the honest answer for "this store is not wired up yet"
	// and the same posture StorefrontConfig.StorefrontToken takes.
	domain := strings.TrimSpace(bindingString(site.Binding, "storeDomain"))
	if domain == "" || !validHost(domain) {
		return out
	}
	origin := "https://" + domain
	out.connect = append(out.connect, origin)
	out.img = append(out.img, origin)
	out.media = append(out.media, origin)
	return out
}

// join appends sources to a directive that already has at least one, or
// returns the directive untouched when there are none.
func (s storefrontCSP) join(directive string, sources []string) string {
	if len(sources) == 0 {
		return directive
	}
	return directive + " " + strings.Join(sources, " ")
}
