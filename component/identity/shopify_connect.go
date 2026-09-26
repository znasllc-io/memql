package identity

import (
	"context"
	"net/url"
	"strings"
)

// shopify_connect.go -- what the identity node's Connect Shopify callback
// (component/identity/http, GET /auth/shopify/callback) shares with the
// Shopify code it cannot import (design record 2026-09-23-connect-shopify,
// 12.6, 12.7).
//
// component/identity is its own module, below integrations/, so the callback
// reaches integrations/shopify through the interface below, wired in
// app/integrations_identity.go. The types live HERE rather than beside the
// handler because the implementation names them in its method signatures, and
// integrations/shopify already imports this package and nothing under it.

// ShopifyConnect is Connect Shopify's half of the callback: the checks and
// writes that need the Shopify store, its sealed secrets and Shopify itself.
// The handler owns the transport, the state row, the audit and the redirect;
// it calls these IN ORDER, and never the next when one refuses.
type ShopifyConnect interface {
	// VerifyShopifyCallback is steps 4 and 5, BEFORE the state is spent: the
	// query's HMAC under the client secret the state's credentialSource names
	// for its shopDomain, and `shop` naming that same shop. False is
	// signature_invalid, and a forged request therefore cannot burn a state.
	VerifyShopifyCallback(ctx context.Context, state *GithubConnectStateRow, query url.Values) bool

	// AuthorizeShopifyConnect is steps 7 to 10, on a SPENT state: re-derive
	// the site and shop, re-check the person under their real role, exchange
	// the code, check the scopes. A non-empty result is the refusal token to
	// redirect with; the grant still names the store when it is known, for
	// the audit.
	AuthorizeShopifyConnect(ctx context.Context, state *GithubConnectStateRow, code string) (ShopifyConnectGrant, string)

	// WriteShopifyConnect is steps 11 to 15: seal, store row, Storefront
	// token, attach, webhooks. It answers the result token the person is sent
	// back with, and what it KEPT: "connected" or "reconnected" once steps 11
	// and 12 landed -- the store's credentials changed -- whatever ended the
	// steps after them, or "" when nothing was kept. The audit reads the
	// second: a credential change is recorded as one, never as a refusal.
	WriteShopifyConnect(ctx context.Context, state *GithubConnectStateRow, grant ShopifyConnectGrant) (result, kept string)
}

// ShopifyConnectGrant is what an authorized callback hands the writes.
type ShopifyConnectGrant struct {
	// Managed connections exchange and seal the expiring token pair under one
	// distributed lock. None of these values appears in String or a result.
	AuthorizationCode string
	OfflineGrant      string
	// StoreID is the v1:shopify:store id the state's shop derives.
	StoreID string
	// AccessToken is the offline Admin API token Shopify issued. Sealed by
	// the writes; never logged, audited or put in an error.
	AccessToken string
	// ClientSecret is the app's client secret the code was exchanged with --
	// which Shopify accepting is the proof it is the approved app's. On a
	// pending approval it is the value the writes promote to the webhook
	// secret, carried rather than re-read so a Save landing after the approval
	// cannot become the live secret unapproved (D12). Never logged, audited or
	// put in an error; String leaves it out with the token.
	ClientSecret string
	// Scopes is every scope Shopify said it granted, as it said them.
	Scopes []string
	// Role is the person's role as their row read at step 8: the subject the
	// attach in step 14 runs under.
	Role string
}

// String keeps the token out of any %v, %+v or slog line a grant reaches.
func (g ShopifyConnectGrant) String() string {
	return "ShopifyConnectGrant{store=" + g.StoreID + ", scopes=" + strings.Join(g.Scopes, ",") + ", role=" + g.Role + "}"
}

// ShopifyResultParam and ShopifySiteParam are the query keys MemQL OS reads a
// Connect Shopify round trip from. Wire contract with the OS (PR 8).
const (
	ShopifyResultParam = "shopify"
	ShopifySiteParam   = "site"
)

// ShopifyDefaultReturnPath is where a Connect Shopify outcome lands when no
// verified state names a path: the Deployables app.
const ShopifyDefaultReturnPath = "/?connect=deployables"

// ShopifyReturnURL composes the OS URL for one Connect Shopify outcome: the
// sibling of GithubReturnURL, with the same re-validation of a client-supplied
// path, plus the storefront the outcome is about. siteID "" names none, which
// is every outcome with no verified state behind it.
func ShopifyReturnURL(osOrigin, returnPath, result, siteID string) string {
	path := SafeRelativeRedirect(returnPath)
	if path == "" {
		path = ShopifyDefaultReturnPath
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	marked := path + sep + ShopifyResultParam + "=" + url.QueryEscape(result)
	if siteID = strings.TrimSpace(siteID); siteID != "" {
		marked += "&" + ShopifySiteParam + "=" + url.QueryEscape(siteID)
	}
	if base := strings.TrimRight(strings.TrimSpace(osOrigin), "/"); base != "" {
		return base + marked
	}
	return marked
}
