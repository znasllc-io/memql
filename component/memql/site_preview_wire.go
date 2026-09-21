package memql

import (
	"net/url"
	"strings"
	"time"
)

// site_preview_wire.go -- the spellings the edge and the mint must agree on, in
// ONE place (epic memql#5531, issue memql#5545).
//
// The path, the query parameter and the cookie name are a protocol between two
// packages that never call each other: integrations/sitepreview composes the
// URL an operator opens, and component/edge is what answers it. Spelled twice
// they would drift, and the drift is the worst kind to diagnose -- a preview
// link that falls through to the SPA fallback and renders the SERVING version,
// which looks exactly like "the candidate did not publish".

const (
	// PreviewEnterPath is where a preview link lands. It sits UNDER /_memql/ so the
	// origin keeps one reserved prefix rather than two -- but the edge answers
	// it ITSELF, ahead of the apiProxy branch, because a preview has to work on
	// a deployable whose apiProxy is off. See component/edge/preview.go.
	PreviewEnterPath = "/_memql/preview"

	// PreviewLeavePath ends a preview in the browser that holds it: it clears the
	// cookie and sends the person back to the public site. It is deliberately
	// NOT revocation -- the grant row is untouched, and a link somebody else
	// holds goes on working. Ending a preview everywhere is revokeSitePreviewGrant.
	PreviewLeavePath = "/_memql/preview/leave"

	// PreviewGrantParam carries the token on the way in, and ONLY on the way in. The
	// edge answers the enter path with a redirect to "/", so the token is in
	// the address bar for one navigation and is a cookie thereafter -- which is
	// what keeps it out of every Referer header the page's own assets send.
	PreviewGrantParam = "grant"

	// PreviewCookieName is what the browser holds afterwards. Host-only, Secure,
	// HttpOnly, SameSite=Lax, Path=/ -- see component/edge/preview.go for why
	// each of those is the only defensible value here.
	PreviewCookieName = "memql_preview"
)

// DefaultPreviewGrantTTL is how long a preview lasts when nothing says otherwise.
//
// THIRTY MINUTES, and the number is about what a preview IS. It is a bearer
// credential for an unpublished version sitting in a URL, so its lifetime is
// the only bound it has; and it is also a working session in which somebody
// walks a catalog, a cart and a hosted checkout, so a lifetime that expired
// mid-walk would train people to keep a longer one. Half an hour is long enough
// for the walk the design record describes and short enough that a leaked link
// is worth one walk.
const DefaultPreviewGrantTTL = 30 * time.Minute

// MaxPreviewGrantTTL caps what an operator may configure.
//
// FOUR HOURS. A cap rather than a free value because the failure it prevents is
// one nobody notices: an operator raises the TTL once to finish a long
// afternoon, and the cluster then hands out day-long bearer tokens for
// unpublished storefronts forever after. A person who needs longer opens
// another preview, which is one click and leaves a record of itself.
const MaxPreviewGrantTTL = 4 * time.Hour

// ClampPreviewGrantTTL folds a configured lifetime into the allowed range.
//
// A NON-POSITIVE VALUE IS THE DEFAULT, NOT "NO EXPIRY". Reading an operator's
// typo or an unset variable as unlimited would remove exactly the bound this
// value exists to provide -- the same reading integrations/customdomain's
// envInt refuses for the same reason.
func ClampPreviewGrantTTL(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return DefaultPreviewGrantTTL
	case d > MaxPreviewGrantTTL:
		return MaxPreviewGrantTTL
	default:
		return d
	}
}

// PreviewEnterURL composes the link an operator opens: the preview entry point on the
// deployable's OWN origin, carrying the token once.
//
// THE SITE'S OWN ORIGIN IS THE REQUIREMENT, not a convenience (issue
// memql#5545). A storefront's cookies, its cart and Shopify's checkout return
// all assume that origin; a preview served from anywhere else would be a
// different site as far as every one of them is concerned, and the walk would
// fail in ways that say nothing about the candidate.
//
// https IS FORCED. Every host this cluster serves is behind TLS in both the
// cloud and the local parity cluster, and a preview token is a credential:
// composing an http link would put one on the wire, and would also be refused
// by the Secure cookie the edge sets.
func PreviewEnterURL(hostname, token string) string {
	host := strings.TrimSpace(strings.ToLower(hostname))
	q := url.Values{}
	q.Set(PreviewGrantParam, strings.TrimSpace(token))
	return "https://" + host + PreviewEnterPath + "?" + q.Encode()
}

// The three keys the Storefront probe reports its steps under, shared by the
// package that produces them (integrations/shopify) and the one that turns them
// into rows (integrations/sitepreview). They are the wire between two
// capabilities, so they belong beside the rest of the wire rather than being
// spelled twice.
const (
	PreviewStepCatalog  = "catalog"
	PreviewStepCart     = "cart"
	PreviewStepCheckout = "checkout"
)
