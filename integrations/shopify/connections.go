package shopify

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	identity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

const managedCredentialSource = "managed"
const managedClientID = "SHOPIFY_CONNECT_CLIENT_ID"
const managedClientSecret = "SHOPIFY_CONNECT_CLIENT_SECRET"
const managedInstallURL = "SHOPIFY_CONNECT_INSTALL_URL"

// Shared storefront connections ask for the commerce set, not the complete
// mirror's finance, staff, payment-method and historical-order permissions
// (read_all_orders is not requested: a store's history begins with the
// window read_orders reaches). Product-content delivery writes product
// metafields, so the catalog is write_products, which includes its reads.
// write_customers is used only for the wholesale pack's customer tag and for
// creating a customer; asking for it now means one approval by the store
// rather than two. A fresh copy on every call, so a caller cannot edit it.
func managedConnectScopes() []string {
	scopes := append([]string(nil), StorefrontScopes...)
	return append(scopes,
		"write_products", "read_inventory", "read_locations", "read_orders",
		"read_customers", "write_customers", "read_discounts", "read_returns")
}

// connectionScopes is what this store's own connection asks Shopify for: the
// commerce set for a store connected through the shared app, the full Connect
// list for a store connected with its own app. Everything a store's coverage
// is measured against starts here -- a scope the connection never asks for is
// not missing, it was never requested.
func connectionScopes(store Store) []string {
	if store.Managed() {
		return managedConnectScopes()
	}
	return ConnectScopes()
}

// scopesNotApproved is what the store's connection asks for and its recorded
// grant does not reach (a write scope reaches its read, grantIncludes), sorted.
// Nil when no grant is recorded: nothing is known to be missing, and the
// origin's own answer decides.
//
// Reported rather than enforced: a missing scope makes Shopify return null
// for the fields it covers, so the mirror is quietly incomplete rather than
// broken. Naming the gap is the difference between "the customer has no
// phone number" and "we were never allowed to see it".
func scopesNotApproved(store Store) []string {
	if len(store.ScopesGranted) == 0 {
		return nil
	}
	have := grantIncludes(store.ScopesGranted)
	var missing []string
	for _, s := range connectionScopes(store) {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	return uniqueSorted(missing)
}

// Operator configuration is never an ordinary-user form. Both values are
// resolved afresh on the receiving node, not remembered by the node doing Begin.
func (c *Connector) managedApp(ctx context.Context) (string, string, error) {
	client, err := c.namedRowValue(ctx, conceptGlobalVariable, managedClientID, "value")
	if err != nil || client == "" {
		return "", "", err
	}
	sealed, err := c.namedRowValue(ctx, conceptGlobalSecret, managedClientSecret, "encryptedValue")
	if err != nil || sealed == "" {
		return client, "", err
	}
	secret, err := c.stores.Secret(operatorContext(ctx), managedClientSecret)
	return client, secret, err
}
func (i *Integration) handleConnectionProviderStatus(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	client, secret, err := i.connector.managedApp(ctx)
	if err != nil {
		return nil, fmt.Errorf("shopify: connection setup could not be read")
	}
	installURL, err := i.connector.namedRowValue(ctx, conceptGlobalVariable, managedInstallURL, "value")
	if err != nil {
		return nil, fmt.Errorf("shopify: installation setup could not be read")
	}
	if !validManagedInstallURL(installURL, client) {
		installURL = ""
	}
	return resultNode("shopify", map[string]any{"reason": "ok", "configured": client != "" && secret != "" && installURL != "", "installUrl": installURL})
}

type connectionCapabilityAsker interface {
	OrganizationCapable(context.Context, string, string, string) bool
	GlobalDataCapable(context.Context, string) bool
}

func (c *Connector) mayManageConnections(ctx context.Context) bool {
	gate, ok := c.engine.(connectionCapabilityAsker)
	return ok && gate.GlobalDataCapable(ctx, auth.VerbRead) &&
		gate.OrganizationCapable(ctx, "", auth.VerbRead, "app:settings/connections") &&
		gate.OrganizationCapable(ctx, "", auth.VerbExecute, "app:settings/connections")
}

// Connecting an existing store must not replace another person's app grant or
// a differently registered app. An OAuth proof authorizes this app at Shopify;
// it does not authorize changing another MemQL user's deployment credentials.
//
// A store Shopify reported UNINSTALLED, or one shop/redact PURGED, is not
// refused (memql#5638). Its row is keyed by its domain, so a refusal would
// lock the shop out of this app for good, and in Shopify's model a reinstall
// is the shop's new consent: the staff approve the app again, Shopify signs
// the callback, and the write clears what the uninstall and the purge recorded
// (markReconnected) and audits it. The owner and app checks above still hold.
// The refusal this replaced asked for a status the store enum does not have,
// so it never fired.
func (c *Connector) managedTarget(ctx context.Context, shop, clientID string) (ConnectTarget, bool) {
	var target ConnectTarget
	domain, err := NormalizeShopDomain(shop)
	if err != nil || !c.mayManageConnections(ctx) || !c.mayReadStores(ctx) {
		return target, false
	}
	actor, err := callerUserID(ctx)
	if err != nil {
		return target, false
	}
	storeID, err := StoreIDForDomain(domain)
	if err != nil {
		return target, false
	}
	prior, found, err := c.storeByID(operatorContext(ctx), storeID)
	if err != nil {
		return target, false
	}
	if found && (memql.BareShortId(prior.OwnerUserID) != memql.BareShortId(actor) || prior.AppClientID != clientID) {
		return target, false
	}
	return ConnectTarget{ShopDomain: domain, StoreID: storeID, Store: prior, StoreFound: found}, true
}
func (i *Integration) handleAccountConnectBegin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	actor, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	client, secret, err := c.managedApp(ctx)
	if err != nil {
		return nil, fmt.Errorf("shopify: connection setup could not be read")
	}
	if client == "" || secret == "" {
		return beginReply("", "shopify_provider_not_configured")
	}
	// The shop comes only from a recent Shopify-signed App URL launch. Ordinary
	// callers cannot start installation by supplying a domain or an unsigned URL.
	shop, valid := verifiedShopifyLaunch(argString(args, "signedQuery"), secret, c.now())
	if !valid {
		return beginReply("", "signature_invalid")
	}
	target, ok := c.managedTarget(ctx, shop, client)
	if !ok {
		return beginReply("", connectReasonPermissionLost)
	}
	return c.beginConnect(ctx, args, actor, target, client, managedCredentialSource)
}
func (c *Connector) managedPerson(ctx context.Context, state *identity.GithubConnectStateRow) (string, bool) {
	users := &identity.Store{Engine: c.engine, Logger: c.logger}
	user, err := users.LookupUserById(operatorContext(ctx), state.UserId)
	if err != nil || user == nil || !user.Active {
		return "", false
	}
	_, ok := c.managedTarget(personContext(ctx, state.UserId, user.Role), state.ShopDomain, state.ClientID)
	return user.Role, ok
}

func (c *Connector) savePersonalConnection(ctx context.Context, state *identity.GithubConnectStateRow, storeID, role string) bool {
	person := personContext(ctx, state.UserId, role)
	result, err := c.engine.Execute(person, "query externalConnectionsMine()")
	if err != nil {
		return false
	}
	connectionID := id.NewShortId()
	for _, row := range memql.MaterializeRows(result) {
		if mapString(row, "provider") == "shopify" && mapString(row, "resourceId") == storeID {
			connectionID = mapString(row, "id")
			break
		}
	}
	// The verified callback supplies internal origin; ownership remains the
	// person's authenticated identity and is stamped by the mutation itself.
	_, err = c.engine.Execute(auth.ContextWithInternalOrigin(person), renderCall("recordExternalConnection", map[string]any{
		"connectionId": connectionID, "provider": "shopify", "resourceId": storeID, "label": state.ShopDomain,
	}))
	return err == nil
}

func (c *Connector) writeManagedConnection(ctx context.Context, state *identity.GithubConnectStateRow, grant identity.ShopifyConnectGrant) (string, string) {
	unlock, err := c.acquireConnectApp(ctx, "offline:"+grant.StoreID)
	if err != nil {
		return connectReasonExchangeFailed, ""
	}
	defer unlock()
	// Repeat the checks under the same cross-node lock used by token refresh.
	role, ok := c.managedPerson(ctx, state)
	if !ok {
		return connectReasonPermissionLost, ""
	}
	bundle, err := c.exchangeOffline(ctx, state.ShopDomain, state.ClientID, grant.ClientSecret, grant.AuthorizationCode, "")
	if err != nil {
		return connectReasonExchangeFailed, ""
	}
	scopes := strings.Split(bundle.Scope, ",")
	if len(missingStorefrontScopes(scopes)) != 0 {
		return connectReasonScopesMissing, ""
	}
	grant.Role, grant.AccessToken, grant.Scopes = role, bundle.AccessToken, scopes
	grant.OfflineGrant = bundle.encoded()
	result, kept := c.writeConnect(ctx, state, grant)
	if kept != "" && !c.savePersonalConnection(ctx, state, grant.StoreID, role) {
		return connectReasonExchangeFailed, kept
	}
	return result, kept
}

// Install links are operator configuration, never a caller-selected redirect.
// The Dev Dashboard install link is for sandbox testing only; merchants use the reviewed listing.
func validManagedInstallURL(raw, clientID string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return false
	}
	switch u.Hostname() {
	case "apps.shopify.com":
		return len(strings.Trim(u.Path, "/")) > 0 && u.RawQuery == ""
	case "admin.shopify.com":
		// Exact development-install link emitted by Shopify's Dev Dashboard.
		q := u.Query()
		if u.Path != "/" || len(q) != 3 || q.Get("no_redirect") != "true" {
			return false
		}
		for _, values := range q {
			if len(values) != 1 {
				return false
			}
		}
		_, orgErr := strconv.ParseUint(q.Get("organization_id"), 10, 64)
		redirect, err := url.Parse(q.Get("redirect"))
		if orgErr != nil || err != nil || redirect.IsAbs() || redirect.Host != "" || redirect.Fragment != "" || redirect.Path != "/oauth/redirect_from_developer_dashboard" {
			return false
		}
		inner := redirect.Query()
		return len(inner) == 1 && len(inner["client_id"]) == 1 && clientID != "" && inner.Get("client_id") == clientID
	}
	return false
}

func verifiedShopifyLaunch(raw, secret string, now time.Time) (string, bool) {
	if len(raw) > 8192 {
		return "", false
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", false
	}
	for _, values := range q {
		if len(values) != 1 {
			return "", false
		}
	}
	// An OAuth callback must go through the session-bound HTTP completion route.
	if q.Has("code") || q.Has("state") || !validCallbackHMAC(q, secret) {
		return "", false
	}
	stamp, err := strconv.ParseInt(q.Get("timestamp"), 10, 64)
	if err != nil || stamp < now.Add(-5*time.Minute).Unix() || stamp > now.Add(time.Minute).Unix() {
		return "", false
	}
	shop, err := NormalizeShopDomain(q.Get("shop"))
	return shop, err == nil
}
