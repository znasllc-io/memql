package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
)

// GET /auth/shopify/callback -- where Shopify sends a person's browser back
// from Connect Shopify (design record 2026-09-23-connect-shopify, 12.6, D2,
// D10). Its neighbour /auth/github/callback's class and placement: Shopify
// redirects a BROWSER here, the flow STARTED over the stream
// (shopifyConnectBegin), and the route is on identity's own table rather than
// in component/server, which would publish it on api.<domain> against the bff
// (github_callback.go says why at length). The identity host routes `/` to
// identity, so no front-door rule is needed either.
//
// # The order of checks is the security of this file
//
//  1. HTTPS.
//  2. No code and no state is Shopify opening the App URL: nothing to finish,
//     so nothing is read, written or echoed.
//  3. The state is LOOKED UP, NOT SPENT. Unknown, another flow's, spent,
//     expired, or a lifetime no server writes: connect_state_invalid. And it
//     must have been begun by THIS browser's live session (D16, GitHub
//     Connect's githubSessionMatches): connect_state_invalid otherwise, with
//     nothing spent.
//  4. and 5. Shopify's signature, under the client secret the state names,
//     and `shop` naming the state's shop -- BEFORE the spend, so a forged
//     request cannot burn a Connect somebody began (the Shopify half).
//  6. The spend, under the advisory lock, which re-checks inside it.
//  7. to 10. The Shopify half re-derives the site and shop, re-checks the
//     person under their real role, exchanges the code and checks the scopes.
//  11. to 15. Then, and only then, the writes (the Shopify half).
//  16. One audit event, whichever way it ended: a credential change as one
//     (shopify_connected / shopify_reconnected, a success) even when a step
//     after it could not finish, a refusal only when nothing was kept.
//  17. A 303 to MemQL OS. Until a state is verified the redirect names no
//     site and no path of the state's: an unverified request learns nothing.

// Connect Shopify's result tokens, under identity.ShopifyResultParam. The ones
// this callback shares with GitHub Connect are github_callback.go's constants
// (connected, reconnected, installed, connect_state_invalid, exchange_failed).
// component/packages catalogues every refusal ("raised on the identity node,
// catalogued here"); spelled as literals because this module sits below it.
const (
	// resultSignatureInvalid: the query did not carry Shopify's signature for
	// the state's shop. Nothing was spent.
	resultSignatureInvalid = "signature_invalid"
	// resultPermissionLost: the person who began no longer may -- their row is
	// gone or inactive, or under their role now they lack the store part, the
	// store, or the site.
	resultPermissionLost = "permission_lost"
	// resultScopesMissing: Shopify granted less than the storefront needs.
	resultScopesMissing = "scopes_missing"
	// resultStorefrontTokenFailed: connected, and the Storefront token could
	// not be minted (step 13); pressing Connect again retries only the mint.
	resultStorefrontTokenFailed = "storefront_token_failed"
)

// shopifyStoreTargetType is the audit targetType: a store is the deployment's
// configuration. A LITERAL, for identity_audit_enum_contract_test.go.
const shopifyStoreTargetType = "shopifyStore"

// maxLoggedShopLength bounds an unverified `shop` in a log line.
const maxLoggedShopLength = 255

func (s *Server) handleShopifyCallback(w http.ResponseWriter, r *http.Request) {
	// 1.
	if !s.requireSecureRequest(w, r) {
		return
	}
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("code"))

	// 2. Shopify opening the App URL. `shop` is logged, never trusted: an
	// unsigned landing names whatever its sender typed.
	if state == "" && code == "" {
		if s.Logger != nil {
			shop := q.Get("shop")
			if len(shop) > maxLoggedShopLength {
				shop = shop[:maxLoggedShopLength]
			}
			s.Logger.Info("identity: Shopify opened the Connect Shopify app URL; nothing to finish", "shopUnverified", shop)
		}
		s.redirectShopify(w, r, "", resultInstalled, "")
		return
	}
	hook := s.ShopifyConnect
	if hook == nil || s.Store == nil {
		// A node with no Shopify half: the route does not exist here.
		http.NotFound(w, r)
		return
	}

	// 3.
	row, reason := s.lookupShopifyState(r, state)
	if reason != "" {
		s.auditShopifyConnect(r, "shopify_connect_refused", "", "", map[string]any{"reason": reason})
		s.redirectShopify(w, r, "", resultStateInvalid, "")
		return
	}

	// 3, continued (D16). The state is bound to the browser session that
	// pressed Connect, exactly as GitHub Connect's is and through its check
	// (githubSessionMatches: the HTTP-only refresh cookie must name that live,
	// unrevoked session of the state's person). Asked BEFORE the signature and
	// the spend, so a link finished from another browser -- somebody else's,
	// or after the session ended -- spends nothing and learns nothing: the
	// person who began can still finish from where they began.
	if !s.githubSessionMatches(r, row) {
		s.auditShopifyConnect(r, "shopify_connect_refused", "", "", map[string]any{"reason": "session_invalid"})
		s.redirectShopify(w, r, "", resultStateInvalid, "")
		return
	}

	// 4. and 5.
	if !hook.VerifyShopifyCallback(r.Context(), row, q) {
		s.auditShopifyConnect(r, "shopify_connect_refused", "", "", map[string]any{"reason": resultSignatureInvalid})
		s.redirectShopify(w, r, "", resultSignatureInvalid, "")
		return
	}

	// 6. Verified: from here the state's own path and site are the person's.
	spent, err := s.Store.ConsumeGithubConnectStateFor(r.Context(), identity.HashConnectState(state), clientIP(r), githubconnect.PurposeShopifyConnect)
	if err != nil || spent == nil {
		s.auditShopifyConnect(r, "shopify_connect_refused", row.UserId, "", map[string]any{"reason": githubConnectRefusalReason(err)})
		s.redirectShopify(w, r, row.ReturnPath, resultStateInvalid, row.SiteID)
		return
	}

	// 7. to 10., then 11. to 15.
	grant, result := hook.AuthorizeShopifyConnect(r.Context(), spent, code)
	kept := ""
	if result == "" {
		result, kept = hook.WriteShopifyConnect(r.Context(), spent, grant)
	}

	// 16. A credential change is recorded as one, a success, even when a
	// later step could not finish: the store's credentials DID change, and a
	// refusal in the trail would say they did not. What ended it rides
	// detail.reason. A refusal is an outcome where nothing was kept.
	detail := map[string]any{"siteId": spent.SiteID, "shopDomain": spent.ShopDomain}
	action := "shopify_connect_refused"
	switch kept {
	case resultConnected:
		action = "shopify_connected"
	case resultReconnected:
		action = "shopify_reconnected"
	}
	if result != kept {
		detail["reason"] = result
	}
	s.auditShopifyConnect(r, action, spent.UserId, grant.StoreID, detail)

	// 17.
	s.redirectShopify(w, r, spent.ReturnPath, result, spent.SiteID)
}

// lookupShopifyState reads the state WITHOUT spending it, answering the row or
// an audit reason. Every reason is one code to the person; the trail keeps
// them apart, as githubConnectRefusalReason does.
func (s *Server) lookupShopifyState(r *http.Request, state string) (*identity.GithubConnectStateRow, string) {
	if state == "" {
		return nil, "state_unknown"
	}
	row, err := s.Store.LookupGithubConnectState(r.Context(), identity.HashConnectState(state))
	switch {
	case err != nil:
		return nil, "state_unreadable"
	case row == nil || !row.IsFor(githubconnect.PurposeShopifyConnect) || !row.ServerShaped():
		return nil, "state_unknown"
	case !row.ConsumedAt.IsZero():
		return nil, "state_already_consumed"
	case time.Now().UTC().After(row.ExpiresAt):
		return nil, "state_expired"
	}
	return row, ""
}

// redirectShopify is the 303 back to MemQL OS (identity.ShopifyReturnURL).
func (s *Server) redirectShopify(w http.ResponseWriter, r *http.Request, returnPath, result, siteID string) {
	http.Redirect(w, r, identity.ShopifyReturnURL(s.osOrigin(r), returnPath, result, siteID), http.StatusSeeOther)
}

// auditShopifyConnect records one outcome: configuration, on the store, with
// fixed tokens and the state's own shop and site -- never a token, secret,
// code or response body.
func (s *Server) auditShopifyConnect(r *http.Request, action, userId, storeID string, detail map[string]any) {
	if s == nil || s.Audit == nil {
		return
	}
	outcome := identity.AuditOutcomeSuccess
	if strings.Contains(action, "refused") {
		outcome = identity.AuditOutcomeFailure
	}
	s.audit(r, identity.AuditEvent{
		Category:    identity.AuditCategoryConfiguration,
		Action:      action,
		TargetType:  shopifyStoreTargetType,
		TargetId:    storeID,
		ActorUserId: userId,
		Outcome:     outcome,
		Detail:      detail,
	})
}
