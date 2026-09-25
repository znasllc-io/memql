package shopify

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"strings"
	"time"

	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// connect_write.go -- what the identity node's Connect Shopify callback keeps
// once Shopify has approved: steps 11 to 15 of design record
// 2026-09-23-connect-shopify, 12.6.
//
// Every write is keyed by the store id, so a callback that fails between two
// of them is repaired by pressing Connect again. The order is chosen so that is
// true at every step: the secrets are sealed before the row points at them,
// the row is written before the pending app is cleared (a failure between the
// two leaves the pending app to Connect with, not a store half-moved to it),
// and the Storefront token is minted only once the Admin connection is kept.
//
// The store's rows are the deployment's, so they are written under
// operatorContext -- never connectorContext, which the store concept refuses
// silently. The ATTACH is the one write judged as the person: the capability
// gate and the binding guards run against their real role (step 8's subject).
//
// Nothing here is read through the StoreRegistry (12.9): its 30-second cache on
// this replica can predate a change another replica made seconds ago.

// Descriptions the rows Connect writes carry about themselves.
const (
	connectSealDescription    = "Sealed by Connect Shopify after the shop's staff approved the app in Shopify."
	connectMintDescription    = "A Storefront API token Connect Shopify minted for the store's storefronts."
	connectClearedDescription = "Cleared by Connect Shopify: the pending app was approved and promoted."
)

// storefrontTokenTitle names the token Connect mints, so the shop's staff can
// tell it apart in Shopify's list (at most 100 per app per shop).
const storefrontTokenTitle = "MemQL storefront"

const shopPlanQuery = `query ShopifyConnectPlan { shop { plan { publicDisplayName partnerDevelopment } } }`

const storefrontTokenCreateMutation = `mutation ShopifyStorefrontTokenCreate($input: StorefrontAccessTokenInput!) {
  storefrontAccessTokenCreate(input: $input) {
    storefrontAccessToken { accessToken }
    userErrors { field message }
  }
}`

// WriteShopifyConnect is steps 11 to 15, asked only after
// AuthorizeShopifyConnect handed over a token. It answers the result the person
// is sent back with -- connected, reconnected, or the fixed refusal of the step
// that could not finish -- and what it kept: connected or reconnected once steps
// 11 and 12 landed, whatever ended it after them, else "".
func (c *Connector) WriteShopifyConnect(ctx context.Context, state *componentIdentity.GithubConnectStateRow, grant componentIdentity.ShopifyConnectGrant) (string, string) {
	if state == nil {
		return connectReasonStateInvalid, ""
	}
	if state.CredentialSource == managedCredentialSource {
		return c.writeManagedConnection(ctx, state, grant)
	}
	return c.writeConnect(ctx, state, grant)
}
func (c *Connector) writeConnect(ctx context.Context, state *componentIdentity.GithubConnectStateRow, grant componentIdentity.ShopifyConnectGrant) (string, string) {
	storeID, err := StoreIDForDomain(state.ShopDomain)
	managed := state.CredentialSource == managedCredentialSource
	promoted := state.CredentialSource == credentialSourcePending || managed
	if err != nil || grant.AccessToken == "" || (promoted && grant.ClientSecret == "") {
		return connectReasonExchangeFailed, ""
	}
	opCtx := operatorContext(ctx)
	person := state.UserId

	c.stores.Invalidate()
	defer c.stores.Invalidate()

	prior, found, err := c.storeByID(opCtx, storeID)
	if err != nil {
		c.logger.Warn("shopify: Connect could not read the store it was about to keep", "store", storeID, "error", err.Error())
		return connectReasonExchangeFailed, ""
	}
	result := connectResultConnected
	if found && prior.AdminTokenRef != "" {
		result = connectResultReconnected
	}

	// 11. The Admin token, and -- only on an approval of the PENDING app (D12)
	// -- its secret promoted to the name the first-boot seed uses. The secret
	// promoted is the grant's, the one the code was exchanged with, and never a
	// re-read of the pending row: a Save landing since the approval would
	// otherwise become the live webhook secret unapproved.
	tokenName, tokenValue := storeSecretName(storeID, suffixAdminToken), grant.AccessToken
	if managed {
		tokenName, tokenValue = storeSecretName(storeID, "OFFLINE_GRANT"), grant.OfflineGrant
	}
	adminRef, err := seedSecret(opCtx, c.engine, tokenName, tokenValue, connectSealDescription, person)
	if err != nil {
		c.logger.Warn("shopify: Connect could not seal the Admin token Shopify issued; nothing was kept", "store", storeID, "error", err.Error())
		return connectReasonExchangeFailed, ""
	}
	// Re-sealing an existing referenced token is already a credential change,
	// even if a later store-row write fails. Preserve that outcome in the audit.
	keptResult := ""
	if found && prior.AdminTokenRef == adminRef {
		keptResult = connectResultReconnected
	}
	scopes := grant.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	update := map[string]any{"storeId": storeID, "adminTokenRef": adminRef, "apiVersion": generated.APIVersion, "scopesGranted": scopes}
	if promoted {
		webhookRef, err := seedSecret(opCtx, c.engine, storeSecretName(storeID, suffixWebhookSecret), grant.ClientSecret, connectSealDescription, person)
		if err != nil {
			c.logger.Warn("shopify: Connect could not promote the approved app's secret; the store is unchanged", "store", storeID, "error", err.Error())
			return connectReasonExchangeFailed, keptResult
		}
		update["appClientId"], update["webhookSecretRef"] = state.ClientID, webhookRef
	}

	// 12. createStore only for a row that does not exist: it re-stamps status,
	// scopes and health. ownerUserId decides whose Library receives privacy
	// exports, so it is the person's only when the row names nobody.
	plan, development := c.shopPlan(opCtx, state.ShopDomain, storeID, grant.AccessToken)
	if plan != "" {
		update["plan"] = plan
	}
	if development != nil {
		update["isDevelopment"] = *development
	}
	if !found {
		create := map[string]any{"domain": state.ShopDomain, "ownerUserId": person}
		for k, v := range update {
			create[k] = v
		}
		if _, err := c.engine.Execute(opCtx, renderCall("createStore", create)); err != nil {
			c.logger.Warn("shopify: Connect could not create the store row", "store", storeID, "error", err.Error())
			return connectReasonExchangeFailed, keptResult
		}
	} else {
		if prior.OwnerUserID == "" {
			update["ownerUserId"] = person
		}
		if _, err := c.engine.Execute(opCtx, renderCall("updateStore", update)); err != nil {
			c.logger.Warn("shopify: Connect could not update the store row", "store", storeID, "error", err.Error())
			return connectReasonExchangeFailed, keptResult
		}
	}
	if promoted && !managed {
		c.clearPendingApp(opCtx, storeID, state.ClientID, grant.ClientSecret, person)
	}

	// 13. A Storefront token only for a store with none.
	storefrontRef := prior.StorefrontTokenRef
	if storefrontRef == "" {
		ref, ok := c.mintStorefrontToken(opCtx, storeID, state.ShopDomain, grant.AccessToken, person)
		if !ok {
			return connectReasonStorefrontTokenFailed, result
		}
		storefrontRef = ref
	}

	// 14.
	if !managed && !c.attachStore(ctx, opCtx, state, storeID, grant.Role) {
		return connectReasonPermissionLost, result
	}

	// 15. Off the request: every subscribed topic is one paced Admin call, and
	// the person's browser is parked on this redirect. A failure does not undo
	// the connection; it lands on the store's subscription health.
	kept := prior
	kept.ID, kept.AdminTokenRef, kept.StorefrontTokenRef, kept.APIVersion = storeID, adminRef, storefrontRef, generated.APIVersion
	if kept.Domain == "" {
		kept.Domain = state.ShopDomain
	}
	run := c.background
	if run == nil {
		run = func(f func()) { go f() }
	}
	detached := context.WithoutCancel(opCtx)
	run(func() {
		ctx, cancel := context.WithTimeout(detached, connectWebhookTimeout)
		defer cancel()
		c.registerConnectWebhooks(ctx, kept)
	})
	return result, result
}

// connectWebhookTimeout bounds step 15 once it no longer has a request to
// end with it.
const connectWebhookTimeout = 10 * time.Minute

// registerConnectWebhooks is step 15: the store's subscriptions reconciled and
// the outcome recorded on its health, which the Store panel shows.
func (c *Connector) registerConnectWebhooks(ctx context.Context, store Store) {
	report, err := c.EnsureSubscriptionsForStore(ctx, store)
	if err != nil {
		c.logger.Warn("shopify: Connect could not read the store's webhook subscriptions", "store", store.ID)
		report.Failed = append(report.Failed, "subscriptions: the store's webhook subscriptions could not be read")
	}
	if err := c.recordSubscriptionHealth(ctx, store, report); err != nil {
		c.logger.Warn("shopify: Connect could not record the store's subscription health", "store", store.ID, "error", err.Error())
	}
}

// storeByID reads one store row by id, directly, as whoever ctx names.
func (c *Connector) storeByID(ctx context.Context, storeID string) (Store, bool, error) {
	res, err := c.engine.Execute(ctx, renderCall("storeById", map[string]any{"storeId": storeID}))
	if err != nil {
		return Store{}, false, err
	}
	if rows := memql.MaterializeRows(res); len(rows) > 0 {
		s, ok := storeFromRow(rows[0])
		return s, ok, nil
	}
	return Store{}, false, nil
}

// shopPlan is the shop's plan name, or "" when Shopify would not say: a label
// the store row shows, never a reason to refuse a connection.
func (c *Connector) shopPlan(ctx context.Context, shop, storeID, token string) (string, *bool) {
	resp, err := c.admin.Do(ctx, Store{ID: storeID, Domain: shop, APIVersion: generated.APIVersion}, token, shopPlanQuery, "ShopifyConnectPlan", nil)
	var out struct {
		Shop struct {
			Plan struct {
				PublicDisplayName  string `json:"publicDisplayName"`
				PartnerDevelopment *bool  `json:"partnerDevelopment"`
			} `json:"plan"`
		} `json:"shop"`
	}
	if err == nil {
		err = resp.DecodeInto(&out)
	}
	if err != nil {
		c.logger.Info("shopify: Connect could not read the shop's plan; the store keeps the one it had", "store", storeID)
		return "", nil
	}
	return strings.TrimSpace(out.Shop.Plan.PublicDisplayName), out.Shop.Plan.PartnerDevelopment
}

// clearPendingApp overwrites the pending pair with blanks at their own rows --
// component/identity/store_githubapp.go's removal, secret first so the pair
// stops reading as saved at the first statement. A failure is logged: the next
// Begin then asks Shopify to approve the app it just approved.
//
// Only the pair that was APPROVED is cleared: one that no longer names the
// state's client id and the promoted secret is a Save made since, which waits
// for its own approval rather than being wiped as though it had one.
func (c *Connector) clearPendingApp(ctx context.Context, storeID, clientID, clientSecret, person string) {
	release, err := c.acquireConnectApp(ctx, storeID)
	if err != nil {
		c.logger.Warn("shopify: pending app clear could not acquire its lock", "store", storeID)
		return
	}
	defer release()
	if !c.pendingIs(ctx, storeID, clientID, clientSecret) {
		c.logger.Info("shopify: Connect kept the store and left a newer pending app for its own approval", "store", storeID)
		return
	}
	secretName := storeSecretName(storeID, suffixPendingClientSecret)
	idName := storeSecretName(storeID, suffixPendingClientID)
	secretRow, err := namedRowID(ctx, c.engine, conceptGlobalSecret, secretName, secretRowID(secretName))
	if err == nil {
		_, err = c.engine.Execute(ctx, renderCall("setGlobalSecret", map[string]any{
			"id": secretRow, "name": secretName, "encryptedValue": "", "fingerprint": "", "kind": "vendor_api_key",
			"description": connectClearedDescription, "addedBy": person, "active": false,
		}))
	}
	var varRow string
	if err == nil {
		varRow, err = namedRowID(ctx, c.engine, conceptGlobalVariable, idName, variableRowID(idName))
	}
	if err == nil {
		_, err = c.engine.Execute(ctx, renderCall("setGlobalVariable", map[string]any{
			"id": varRow, "name": idName, "value": "", "description": connectClearedDescription, "active": false,
		}))
	}
	if err != nil {
		c.logger.Warn("shopify: Connect kept the store and could not clear the pending app", "store", storeID, "error", err.Error())
	}
}

// pendingIs reports whether the pending pair still names clientID and
// clientSecret. The secrets are compared as SHA-256 digests in constant time;
// neither value is logged. Any read that fails is a no, which keeps the pair.
func (c *Connector) pendingIs(ctx context.Context, storeID, clientID, clientSecret string) bool {
	id, err := c.pendingClientID(ctx, storeID)
	if err != nil || id == "" || id != clientID {
		return false
	}
	current, err := c.stores.Secret(ctx, storeSecretName(storeID, suffixPendingClientSecret))
	if err != nil || current == "" {
		return false
	}
	have, want := sha256.Sum256([]byte(current)), sha256.Sum256([]byte(clientSecret))
	return subtle.ConstantTimeCompare(have[:], want[:]) == 1
}

// mintStorefrontToken is step 13: mint, seal, and point the store at it
// straight away. The token is never logged; a failure after the mint is, because
// that token now counts toward the shop's cap and only Shopify can delete it.
func (c *Connector) mintStorefrontToken(ctx context.Context, storeID, shop, adminToken, person string) (string, bool) {
	resp, err := c.admin.Do(ctx, Store{ID: storeID, Domain: shop, APIVersion: generated.APIVersion}, adminToken,
		storefrontTokenCreateMutation, "ShopifyStorefrontTokenCreate", map[string]any{"input": map[string]any{"title": storefrontTokenTitle}})
	if err == nil {
		err = userErrorsFrom(resp, "storefrontAccessTokenCreate")
	}
	var out struct {
		Create struct {
			Token struct {
				AccessToken string `json:"accessToken"`
			} `json:"storefrontAccessToken"`
		} `json:"storefrontAccessTokenCreate"`
	}
	if err == nil {
		err = resp.DecodeInto(&out)
	}
	minted := strings.TrimSpace(out.Create.Token.AccessToken)
	if err != nil || minted == "" {
		// Shopify's own sentence is part of a response body, so it stays out.
		c.logger.Warn("shopify: Connect could not mint a Storefront token; the Admin connection is kept and Connect again retries only the mint", "store", storeID)
		return "", false
	}
	ref, err := seedSecret(ctx, c.engine, storeSecretName(storeID, suffixStorefrontToken), minted, connectMintDescription, person)
	if err == nil {
		err = c.setStorefrontTokenRef(ctx, storeID, ref)
	}
	if err != nil {
		c.logger.Error("shopify: Connect minted a Storefront token and could not keep it; it counts toward the shop's limit of 100 until it is deleted in Shopify",
			"store", storeID, "error", err.Error())
		return "", false
	}
	return ref, true
}

// attachStore is step 14: the site's serving binding names the store, written
// under the PERSON (step 8's subject) so the capability gate and the binding
// guards judge them. A binding already naming the store is left alone; the
// site's status is never touched, so a draft stays a draft.
func (c *Connector) attachStore(ctx, opCtx context.Context, state *componentIdentity.GithubConnectStateRow, storeID, role string) bool {
	if res, err := c.engine.Execute(opCtx, renderCall("siteById", map[string]any{"siteId": state.SiteID})); err == nil {
		if rows := memql.MaterializeRows(res); len(rows) > 0 {
			binding, _ := rowValue(rows[0], "binding").(map[string]any)
			if memql.BareShortId(mapString(binding, "storeId")) == storeID {
				return true
			}
		}
	}
	call := renderCall("updateSiteStoreBinding", map[string]any{"siteId": state.SiteID, "storeId": storeID})
	if _, err := c.engine.Execute(personContext(ctx, state.UserId, role), call); err != nil {
		c.logger.Warn("shopify: Connect kept the store and the storefront refused the attach", "store", storeID, "site", state.SiteID, "error", err.Error())
		return false
	}
	return true
}
