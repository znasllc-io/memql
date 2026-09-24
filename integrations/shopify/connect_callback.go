package shopify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
)

// connect_callback.go -- Connect Shopify's half of GET /auth/shopify/callback on
// the identity node (design record 2026-09-23-connect-shopify, 12.6, 12.7).
//
// The identity server owns the transport, the state row, the audit and the
// redirect, and asks three methods in order through
// componentIdentity.ShopifyConnect: the two checks below, then the writes
// (connect_write.go). This half owns what needs the store, its
// sealed secrets and Shopify. Every read here is by id, direct, as the
// deployment -- never through the StoreRegistry, whose 30-second cache on this
// replica can predate a Save made seconds ago on another (12.9).
//
// No token, secret, code or response body reaches a log line, an error or a
// result: the results are fixed tokens and the identity server audits those.

// The refusals this half answers, catalogued in component/packages/refusal.go
// beside connect_state_invalid (connectReasonStateInvalid, connect.go). A failed
// signature is the identity server's to answer: Verify only says whether it
// holds.
const (
	connectReasonPermissionLost        = "permission_lost"
	connectReasonExchangeFailed        = "exchange_failed"
	connectReasonScopesMissing         = "scopes_missing"
	connectReasonStorefrontTokenFailed = "storefront_token_failed"
)

// The two successes the writes answer (connect_write.go): a store connected for
// the first time, and one that already held an Admin token.
const (
	connectResultConnected   = "connected"
	connectResultReconnected = "reconnected"
)

// maxConnectExchangeBody bounds what Shopify's token endpoint may send back: a
// small JSON object, and an unbounded read against a third party is a memory
// decision made by them.
const maxConnectExchangeBody = 1 << 20

// VerifyShopifyCallback is steps 4 and 5, BEFORE the state is spent: the query
// carries Shopify's HMAC under the client secret the state names, and its
// `shop` is the state's shop.
func (c *Connector) VerifyShopifyCallback(ctx context.Context, state *componentIdentity.GithubConnectStateRow, query url.Values) bool {
	if state == nil {
		return false
	}
	secret, ok := c.connectSecret(ctx, state)
	if !ok || !validCallbackHMAC(query, secret) {
		return false
	}
	shop, err := NormalizeShopDomain(query.Get("shop"))
	return err == nil && shop == state.ShopDomain
}

// validCallbackHMAC is Shopify's callback signature: every parameter but hmac,
// sorted by key and joined key=value with & -- url.Values.Encode, which is the
// form-encoded string Shopify's own client libraries sign -- under HMAC-SHA256,
// compared in constant time on the decoded bytes.
func validCallbackHMAC(query url.Values, secret string) bool {
	given, err := hex.DecodeString(query.Get("hmac"))
	if err != nil || len(given) != sha256.Size || secret == "" {
		return false
	}
	rest := url.Values{}
	for k, v := range query {
		if k != "hmac" {
			rest[k] = v
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(rest.Encode()))
	return hmac.Equal(mac.Sum(nil), given)
}

// connectSecret is the client secret a state's credentialSource names for its
// shop: the pending one a Save sealed, or the store's current webhook secret
// (one secret, three uses). ok=false is "nothing to verify or exchange with".
func (c *Connector) connectSecret(ctx context.Context, state *componentIdentity.GithubConnectStateRow) (string, bool) {
	storeID, err := StoreIDForDomain(state.ShopDomain)
	if err != nil {
		return "", false
	}
	opCtx := operatorContext(ctx)
	var name string
	switch state.CredentialSource {
	case credentialSourcePending:
		name = storeSecretName(storeID, suffixPendingClientSecret)
	case credentialSourceCurrent:
		s, found, err := c.storeByID(opCtx, storeID)
		if err != nil {
			c.logger.Warn("shopify: Connect could not read the store it verifies for", "store", storeID)
			return "", false
		}
		if !found {
			return "", false
		}
		name = s.WebhookSecretRef
	}
	if name == "" {
		return "", false
	}
	secret, err := c.stores.Secret(opCtx, name)
	if err != nil || secret == "" {
		c.logger.Info("shopify: Connect found no client secret to verify with", "store", storeID, "source", state.CredentialSource)
		return "", false
	}
	return secret, true
}

// AuthorizeShopifyConnect is steps 7 to 10, on a SPENT state.
func (c *Connector) AuthorizeShopifyConnect(ctx context.Context, state *componentIdentity.GithubConnectStateRow, code string) (componentIdentity.ShopifyConnectGrant, string) {
	var grant componentIdentity.ShopifyConnectGrant
	if state == nil {
		return grant, connectReasonStateInvalid
	}
	grant.StoreID, _ = StoreIDForDomain(state.ShopDomain)

	// 7. Re-derive, don't trust: the site is still a storefront and 12.1 still
	// gives the state's shop. Asked as the deployment, because the question is
	// about the site; the person is step 8's.
	target, reason, err := c.ResolveConnectSite(operatorContext(ctx), state.SiteID)
	if err != nil || reason != "" || target.ShopDomain != state.ShopDomain {
		if err != nil {
			c.logger.Warn("shopify: Connect could not re-derive the storefront", "store", grant.StoreID, "error", err.Error())
		}
		return grant, connectReasonStateInvalid
	}

	// 8. The person, now, under their real role.
	role, ok := c.personMayConnect(ctx, state.UserId, target)
	if !ok {
		return grant, connectReasonPermissionLost
	}

	// 9.
	secret, ok := c.connectSecret(ctx, state)
	if !ok {
		return grant, connectReasonExchangeFailed
	}
	token, scopes, err := c.exchangeConnectCode(ctx, state.ShopDomain, state.ClientID, secret, code)
	if err != nil {
		c.logger.Info("shopify: Connect's code exchange failed", "store", grant.StoreID, "reason", err.Error())
		return grant, connectReasonExchangeFailed
	}

	// 10. Only a missing Storefront scope refuses; a missing Admin read scope
	// is recorded and shown.
	if missing := missingStorefrontScopes(scopes); len(missing) > 0 {
		c.logger.Info("shopify: Connect was granted less than the storefront needs", "store", grant.StoreID, "missing", strings.Join(missing, ","))
		return grant, connectReasonScopesMissing
	}
	grant.AccessToken, grant.ClientSecret, grant.Scopes, grant.Role = token, secret, scopes, role
	return grant, ""
}

// personMayConnect is step 8: the person's row is read now and must be active,
// and under THEIR REAL ROLE -- never auth.ContextWithUserActor, whose synthetic
// Unranked writer holds no store part and ranks below the store's read floor --
// the store reads back, the site reads back, they hold the store part (theirs
// and at the site's organization), and the site is writable. Any read that
// fails is a no: this gate stands before a store's credentials change.
//
// It judges, BEFORE any write, what step 14's attach will judge after them. A
// store with no row yet (a first Connect) has nothing to read back, so the
// store's read TIER is asked instead (MayReadConcept): the question the binding
// guard asks of the row step 12 is about to create. Skipping it would let a
// person granted the store part but ranked below the store's read floor seal
// the Admin token and create the store before the attach refused them.
func (c *Connector) personMayConnect(ctx context.Context, userID string, target ConnectTarget) (string, bool) {
	if strings.TrimSpace(userID) == "" {
		return "", false
	}
	store := &componentIdentity.Store{Engine: c.engine, Logger: c.logger}
	user, err := store.LookupUserById(operatorContext(ctx), userID)
	if err != nil || user == nil || !user.Active {
		return "", false
	}
	subject := personContext(ctx, userID, user.Role)
	if target.StoreFound {
		res, err := c.engine.Execute(subject, renderCall("storeById", map[string]any{"storeId": target.StoreID}))
		if err != nil || len(memql.MaterializeRows(res)) == 0 {
			return "", false
		}
	} else if !c.mayReadStores(subject) {
		return "", false
	}
	res, err := c.engine.Execute(subject, renderCall("siteById", map[string]any{"siteId": target.SiteID}))
	sites := memql.MaterializeRows(res)
	if err != nil || len(sites) == 0 {
		return "", false
	}
	if !c.mayAttachStore(subject, mapString(sites[0], "accountId"), target.StoreID) {
		return "", false
	}
	writable, err := c.mayWriteRow(subject, conceptPlatformSite, target.SiteID)
	if err != nil || !writable {
		return "", false
	}
	return user.Role, true
}

// conceptReadAdmitter is the engine's row-free read answer (memql.MemQLEngine's
// MayReadConcept), asked as an optional interface for rowWriteAdmitter's reason.
type conceptReadAdmitter interface {
	MayReadConcept(ctx context.Context, conceptName string) (bool, error)
}

// mayReadStores is whether the subject may read v1:shopify:store rows at all:
// the store tier's answer, which needs no row. An engine that cannot say, or a
// read that fails, is a no.
func (c *Connector) mayReadStores(subject context.Context) bool {
	a, ok := c.engine.(conceptReadAdmitter)
	if !ok {
		return false
	}
	readable, err := a.MayReadConcept(subject, conceptShopifyStore)
	return err == nil && readable
}

// storeBindingAsker is the engine's own store-part pre-check
// (memql.MemQLEngine's MayChangeStoreBinding), asked as an optional interface
// for rowWriteAdmitter's reason.
type storeBindingAsker interface {
	MayChangeStoreBinding(ctx context.Context, accountId, priorStoreId, storeId string) bool
}

// mayAttachStore is step 8's store part: may the subject attach storeID to a
// site of accountID. It asks the two guards a binding write meets -- the
// caller's own part and the part at the site's organization (memql#5598) --
// as though the site were bound to NOTHING. The guards judge only a change,
// and a reconnect of a site already bound to this store changes no binding,
// so passing the real prior binding would let a person who lost the part
// re-credential the store unasked.
func (c *Connector) mayAttachStore(subject context.Context, accountID, storeID string) bool {
	a, ok := c.engine.(storeBindingAsker)
	return ok && a.MayChangeStoreBinding(subject, accountID, "", storeID)
}

// personContext is the person as a request of theirs would carry them: claims,
// token and access context from their row's real role, ranked and not
// synthetic, and a CLIENT call -- internal origin would pass every gate this
// subject exists to be judged by. The shape of
// component/memql/platform_site_preview_guard.go's reader.
func personContext(ctx context.Context, userID, role string) context.Context {
	claims := map[string]any{"sub": userID, "role": role}
	ctx = auth.ContextWithClientOrigin(ctx)
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: userID, Role: auth.Role(strings.ToLower(strings.TrimSpace(role)))})
}

// errConnectExchange is every exchange failure's error: fixed text, so no body,
// secret or code can ride it into a log.
var errConnectExchange = errors.New("shopify: the code exchange returned no token")

// exchangeConnectCode is step 9, githubconnect.Client.ExchangeCode's shape: a
// form-encoded POST to the STATE'S shop with client_id, client_secret and code
// in the body, at most 1 MiB read back, and no redirect followed -- a 307 would
// re-send the body, secret included, wherever it pointed.
func (c *Connector) exchangeConnectCode(ctx context.Context, shop, clientID, clientSecret, code string) (string, []string, error) {
	if code == "" || clientID == "" || clientSecret == "" {
		return "", nil, errConnectExchange
	}
	endpoint := "https://" + shop + "/admin/oauth/access_token"
	if c.connectTokenURL != nil {
		endpoint = c.connectTokenURL(shop)
	}
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", nil, errConnectExchange
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		// A browser is parked on this redirect.
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("shopify: the code exchange could not reach the shop")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConnectExchangeBody))
	if err != nil {
		return "", nil, errConnectExchange
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("shopify: the code exchange answered %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
	}
	if json.Unmarshal(body, &out) != nil || strings.TrimSpace(out.AccessToken) == "" {
		return "", nil, errConnectExchange
	}
	var scopes []string
	for _, s := range strings.Split(out.Scope, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	return out.AccessToken, scopes, nil
}

// missingStorefrontScopes is the Storefront scopes a grant lacks. A write
// scope implies its read, as Shopify reports and its own client reads them.
func missingStorefrontScopes(granted []string) []string {
	have := map[string]bool{}
	for _, s := range granted {
		have[s] = true
		if i := strings.Index(s, "write_"); i >= 0 {
			have[s[:i]+"read_"+s[i+len("write_"):]] = true
		}
	}
	var missing []string
	for _, s := range StorefrontScopes {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	return missing
}
