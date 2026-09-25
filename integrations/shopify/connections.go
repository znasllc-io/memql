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

// Shared storefront connections need commerce access, not the complete mirror's
// finance, staff, payment-method and historical-order permissions.
func managedConnectScopes() []string {
	scopes := append([]string(nil), StorefrontScopes...)
	return append(scopes, "read_products", "read_inventory", "read_locations", "read_orders")
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
	if found && (memql.BareShortId(prior.OwnerUserID) != memql.BareShortId(actor) ||
		prior.AppClientID != clientID || prior.Status == "redacted") {
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
	_, err = c.engine.Execute(operatorContext(ctx), renderCall("recordExternalConnection", map[string]any{
		"connectionId": connectionID, "ownerUserId": state.UserId, "provider": "shopify", "resourceId": storeID, "label": state.ShopDomain,
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
