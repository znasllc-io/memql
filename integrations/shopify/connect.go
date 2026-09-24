package shopify

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	componentIdentity "github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// connect.go -- Connect Shopify's store resolution and the Store panel's calls
// (design record 2026-09-23-connect-shopify, section 12).
//
// # The shop is never the browser's
//
// Every call names a SITE. ResolveConnectSite turns it into a shop on the
// server: the caller must be able to write the site, the site must be a
// storefront, and the shop is what the package run that last published it
// named. A shop taken from the request would let anybody who can write one
// storefront point it -- and its credentials -- at any store on Shopify.
//
// # The writes run as the deployment, after the checks
//
// A store row and its sealed secrets are the cluster's, not a person's, so they
// are written under operatorContext -- never connectorContext, which the store
// concept refuses silently (connector.go). That is a REQUEST-DERIVED use of an
// internal-origin identity, and what bounds it is order: the engine asks the
// store part at the builtin before any handler here runs, and each handler asks
// ResolveConnectSite before it writes anything. connect_precondition_test.go
// holds both halves.

// Answers a caller can see, catalogued once. `ok` is the only success.
const (
	connectReasonOK                      = "ok"
	connectReasonSiteNotWritable         = "site_not_writable"
	connectReasonNotAStorefront          = "not_a_storefront"
	connectReasonStoreNotNamed           = "store_not_named"
	connectReasonStoreRedacted           = "store_redacted"
	connectReasonAppCredentialsInvalid   = "app_credentials_invalid"
	connectReasonSecretNameAmbiguous     = "secret_name_ambiguous"
	connectReasonStoreNotConnected       = "store_not_connected"
	connectReasonStoreInUse              = "store_in_use"
	connectReasonStorefrontTokenRequired = "storefront_token_required"
	connectReasonStorefrontTokenInvalid  = "storefront_token_invalid"
	connectReasonShopifyAppNotSaved      = "shopify_app_not_saved"
	connectReasonStateInvalid            = "connect_state_invalid"
)

// The row names a store's credentials live under: SHOPIFY_<ID>_<SUFFIX>, the
// first-boot seed's convention (config.go).
const (
	suffixAdminToken          = "ADMIN_TOKEN"
	suffixStorefrontToken     = "STOREFRONT_TOKEN"
	suffixWebhookSecret       = "WEBHOOK_SECRET"
	suffixPendingClientID     = "PENDING_CLIENT_ID"
	suffixPendingClientSecret = "PENDING_CLIENT_SECRET"
)

const (
	conceptGlobalSecret   = "v1:platform:globalSecret"
	conceptGlobalVariable = "v1:platform:globalVariable"
	conceptPlatformSite   = "v1:platform:site"
	conceptShopifyStore   = "v1:shopify:store"
	kindStorefront        = "shopify_storefront"
)

// storeSecretName is SHOPIFY_<ID>_<SUFFIX>. The id keeps its hyphen, as the seed
// always has; changing that is its own task (design section 15).
func storeSecretName(storeID, suffix string) string {
	return "SHOPIFY_" + strings.ToUpper(storeID) + "_" + suffix
}

// StorefrontScopes are the Storefront API scopes Connect REQUIRES (design 12.8):
// without them the storefront cannot read its catalog or hold a cart, and
// storefrontAccessTokenCreate refuses to mint. Derived from the Storefront calls
// memql-fylo's storefront makes; the runbook prints them.
var StorefrontScopes = []string{
	"unauthenticated_read_product_listings",
	"unauthenticated_read_checkouts",
	"unauthenticated_write_checkouts",
	"unauthenticated_read_customers",
}

// ConnectScopes is the one scope list Connect requests, Storefront scopes first,
// then the Admin read scopes the mirror and webhooks need (generated.Scopes). A
// missing Admin scope is shown, never refused. A copy, so a caller cannot edit it.
func ConnectScopes() []string {
	out := make([]string, 0, len(StorefrontScopes)+len(generated.Scopes))
	out = append(out, StorefrontScopes...)
	return append(out, generated.Scopes...)
}

// ConnectTarget is what ResolveConnectSite learned about one storefront.
type ConnectTarget struct {
	// SiteID is the site row's id as the engine answered it.
	SiteID string
	// ShopDomain is the normalized <shop>.myshopify.com the publishing run named.
	ShopDomain string
	// StoreID is the v1:shopify:store id that shop derives.
	StoreID string
	// Store is the store row, read as the deployment; zero when StoreFound is false.
	Store      Store
	StoreFound bool
}

// ResolveConnectSite is the one answer to "which store does this storefront's
// Connect act on" (design 12.1). It returns a reason instead of a target when
// the caller may not act: "" means resolved.
//
//  1. The site is read UNDER THE CALLER, and must be one they may WRITE -- the
//     row-authz write admission, asked through the engine (MayWriteRow).
//  2. It must be a shopify_storefront.
//  3. The shop is binding.store from the report of the newest run of the site's
//     package whose outcomes carry this site -- the run that last published it
//     (component/packages/report.go). No such run is store_not_named.
//  4. The store row is read BY ID as the deployment: never as the caller, never
//     as the connector, never through the StoreRegistry's cache.
func (c *Connector) ResolveConnectSite(ctx context.Context, siteID string) (ConnectTarget, string, error) {
	siteID = strings.TrimSpace(siteID)
	if siteID == "" {
		return ConnectTarget{}, connectReasonSiteNotWritable, nil
	}
	res, err := c.engine.Execute(ctx, renderCall("siteById", map[string]any{"siteId": siteID}))
	if err != nil {
		return ConnectTarget{}, "", fmt.Errorf("shopify: read site %q: %w", siteID, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return ConnectTarget{}, connectReasonSiteNotWritable, nil
	}
	site := rows[0]
	target := ConnectTarget{SiteID: mapString(site, "id")}
	if target.SiteID == "" {
		target.SiteID = siteID
	}
	writable, err := c.mayWriteRow(ctx, conceptPlatformSite, target.SiteID)
	if err != nil {
		return ConnectTarget{}, "", err
	}
	if !writable {
		return ConnectTarget{}, connectReasonSiteNotWritable, nil
	}
	if mapString(site, "kind") != kindStorefront {
		return ConnectTarget{}, connectReasonNotAStorefront, nil
	}

	named, err := c.publishedStoreName(ctx, mapString(site, "packageId"), mapString(site, "packageDeployableName"), target.SiteID)
	if err != nil {
		return ConnectTarget{}, "", err
	}
	shop, err := NormalizeShopDomain(named)
	if err != nil {
		return ConnectTarget{}, connectReasonStoreNotNamed, nil
	}
	storeID, err := StoreIDForDomain(shop)
	if err != nil {
		return ConnectTarget{}, connectReasonStoreNotNamed, nil
	}
	if !c.mayReadStores(ctx) {
		return ConnectTarget{}, "permission_lost", nil
	}

	target.ShopDomain, target.StoreID = shop, storeID

	res, err = c.engine.Execute(operatorContext(ctx), renderCall("storeById", map[string]any{"storeId": storeID}))
	if err != nil {
		return ConnectTarget{}, "", fmt.Errorf("shopify: read store %q: %w", storeID, err)
	}
	if rows := memql.MaterializeRows(res); len(rows) > 0 {
		if s, ok := storeFromRow(rows[0]); ok {
			target.Store, target.StoreFound = s, true
		}
	}
	if target.StoreFound && target.Store.RedactedAt != "" {
		return ConnectTarget{}, connectReasonStoreRedacted, nil
	}
	return target, "", nil
}

// publishedStoreName is binding.store of the deployable this site serves, from
// the newest run of its package that published it. The runs are read as the
// deployment: the caller's reach was settled on the site, and the run a
// storefront was published by is a fact about the site rather than a second
// question about the caller. Matching on the site id is what keeps a forged
// packageId from borrowing another package's manifest.
//
// ponytail: the newest 50 runs of the package (packageDeployments' page); a
// storefront last published further back than that answers store_not_named until
// it is redeployed.
func (c *Connector) publishedStoreName(ctx context.Context, packageID, deployable, siteID string) (string, error) {
	if packageID == "" || deployable == "" {
		return "", nil
	}
	res, err := c.engine.Execute(operatorContext(ctx), renderCall("packageDeployments", map[string]any{"packageId": packageID}))
	if err != nil {
		return "", fmt.Errorf("shopify: read the runs of package %q: %w", packageID, err)
	}
	want := memql.BareShortId(siteID)
	for _, run := range memql.MaterializeRows(res) {
		if !runPublished(run, want) {
			continue
		}
		report, _ := rowValue(run, "report").(map[string]any)
		entries, _ := report["deployables"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if mapString(entry, "name") != deployable {
				continue
			}
			binding, _ := entry["binding"].(map[string]any)
			return mapString(binding, "store"), nil
		}
		return "", nil
	}
	return "", nil
}

// runPublished reports whether a run's outcomes carry this site.
func runPublished(run map[string]any, bareSiteID string) bool {
	outcomes, _ := rowValue(run, "deployables").([]any)
	for _, o := range outcomes {
		outcome, _ := o.(map[string]any)
		if id := mapString(outcome, "siteId"); id != "" && memql.BareShortId(id) == bareSiteID {
			return true
		}
	}
	return false
}

// rowWriteAdmitter is the engine's own write admission (memql.MemQLEngine's
// MayWriteRow). Asked as an optional interface because the plug-in surface is
// narrower than the engine, and an engine that cannot answer it is refused
// rather than trusted.
type rowWriteAdmitter interface {
	MayWriteRow(ctx context.Context, conceptName, id string) (bool, error)
}

func (c *Connector) mayWriteRow(ctx context.Context, concept, id string) (bool, error) {
	w, ok := c.engine.(rowWriteAdmitter)
	if !ok {
		return false, fmt.Errorf("shopify: this engine cannot say who may write %s rows, so Connect writes nothing", concept)
	}
	return w.MayWriteRow(ctx, concept, id)
}

// namedRowsQuery is the read the secret and variable resolvers make
// (component/memql/engine_variables.go readNamedRowFields), spelled the same way
// so "which rows carry this name" is asked of exactly what the resolver reads.
// Every name passed here is SHOPIFY_<id>_<suffix> over a validated store id.
func namedRowsQuery(concept, name string) string {
	return fmt.Sprintf(`concept==%s&&payload.name==%s`, concept, jsonString(name))
}

// errSecretNameAmbiguous: more than one row carries a name the resolver will
// read the first of, so no write could say which one it changed.
var errSecretNameAmbiguous = errors.New("shopify: more than one row carries this name")

// namedRowID is the id to write a named row at: the one row that carries the
// name, or `derived` when none does. More than one is errSecretNameAmbiguous.
func namedRowID(ctx context.Context, engine memql.IntegrationEngineAccess, concept, name, derived string) (string, error) {
	res, err := engine.Execute(ctx, namedRowsQuery(concept, name))
	if err != nil {
		return "", fmt.Errorf("shopify: look up %s: %w", name, err)
	}
	ids := map[string]bool{}
	for _, row := range memql.MaterializeRows(res) {
		if id := mapString(row, "id"); id != "" {
			ids[memql.BareShortId(id)] = true
		}
	}
	switch len(ids) {
	case 0:
		return derived, nil
	case 1:
		for id := range ids {
			return id, nil
		}
	}
	return "", errSecretNameAmbiguous
}

// namedRowValue is field of the first row carrying the name -- the one the
// resolver reads -- or "" when no row does. Never called for a secret's
// plaintext: a sealed row's fields are ciphertext and a fingerprint.
func (c *Connector) namedRowValue(ctx context.Context, concept, name, field string) (string, error) {
	res, err := c.engine.Execute(operatorContext(ctx), namedRowsQuery(concept, name))
	if err != nil {
		return "", fmt.Errorf("shopify: read %s: %w", name, err)
	}
	if rows := memql.MaterializeRows(res); len(rows) > 0 {
		return mapString(rows[0], field), nil
	}
	return "", nil
}

// pendingClientID is the saved app's client id while a save is waiting for an
// approval, or "": both halves must be present, which is what Begin needs to
// use them.
func (c *Connector) pendingClientID(ctx context.Context, storeID string) (string, error) {
	id, err := c.namedRowValue(ctx, conceptGlobalVariable, storeSecretName(storeID, suffixPendingClientID), "value")
	if err != nil || id == "" {
		return "", err
	}
	sealed, err := c.namedRowValue(ctx, conceptGlobalSecret, storeSecretName(storeID, suffixPendingClientSecret), "encryptedValue")
	if err != nil || sealed == "" {
		return "", err
	}
	return id, nil
}

// callerUserID is the person a write is audited as. A call naming nobody is an
// error rather than a reason: the audit trail would say nobody did it.
func callerUserID(ctx context.Context) (string, error) {
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil && strings.TrimSpace(ac.UserId) != "" {
		return strings.TrimSpace(ac.UserId), nil
	}
	return "", fmt.Errorf("shopify: Connect Shopify needs a signed-in caller")
}

// ---------------------------------------------------------------------------
// shopifyConnectStatus (12.2)
// ---------------------------------------------------------------------------

func (i *Integration) handleConnectStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	out := map[string]any{
		"reason": connectReasonOK, "storeId": "", "shopDomain": "",
		"appSaved": false, "pendingApp": false, "connected": false, "storefrontTokenSet": false,
		"requiredScopes": ConnectScopes(), "grantedScopes": []string{},
	}
	target, reason, err := c.ResolveConnectSite(ctx, argString(args, "siteId"))
	if err != nil {
		return nil, err
	}
	if reason != "" {
		out["reason"] = reason
		return resultNode("shopify", out)
	}
	// "Is there an app to connect with" has one answer, Begin's: pending
	// credentials, or a current app id with its webhook secret row.
	_, source, err := c.connectCredentials(ctx, target)
	if err != nil {
		return nil, err
	}
	s := target.Store
	out["storeId"], out["shopDomain"] = target.StoreID, target.ShopDomain
	out["pendingApp"] = source == credentialSourcePending
	out["appSaved"] = source != ""
	out["connected"] = target.StoreFound && s.AdminTokenRef != ""
	out["storefrontTokenSet"] = target.StoreFound && s.StorefrontTokenRef != ""
	if len(s.ScopesGranted) > 0 {
		out["grantedScopes"] = s.ScopesGranted
	}
	return resultNode("shopify", out)
}

// ---------------------------------------------------------------------------
// shopifyStoreAppSave (12.3)
// ---------------------------------------------------------------------------

// pendingAppDescription is what the pending rows say about themselves.
const pendingAppDescription = "Saved on a storefront's Store panel in MemQL OS. Pending: it takes effect only when the shop's staff approve Connect Shopify."

// maxAppCredentialLength bounds what a Store panel field may carry. Shopify's
// are 32 hex characters and ~38; anything near this is not one.
const maxAppCredentialLength = 256

func (i *Integration) handleStoreAppSave(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	actor, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	target, reason, err := c.ResolveConnectSite(ctx, argString(args, "siteId"))
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return connectReply(reason, ConnectTarget{})
	}
	clientID := argString(args, "clientId")
	clientSecret := argString(args, "clientSecret")
	if clientID == "" || clientSecret == "" || len(clientID) > maxAppCredentialLength || len(clientSecret) > maxAppCredentialLength {
		return connectReply(connectReasonAppCredentialsInvalid, ConnectTarget{})
	}

	release, err := c.acquireConnectApp(ctx, target.StoreID)
	if err != nil {
		return nil, err
	}
	defer release()
	opCtx := operatorContext(ctx)
	idName := storeSecretName(target.StoreID, suffixPendingClientID)
	// Both names are looked up BEFORE either is written, so an ambiguous one
	// refuses the save with nothing changed.
	varID, err := namedRowID(opCtx, c.engine, conceptGlobalVariable, idName, variableRowID(idName))
	if errors.Is(err, errSecretNameAmbiguous) {
		return connectReply(connectReasonSecretNameAmbiguous, ConnectTarget{})
	}
	if err != nil {
		return nil, err
	}
	secretName := storeSecretName(target.StoreID, suffixPendingClientSecret)
	if _, err := seedSecret(opCtx, c.engine, secretName, clientSecret, pendingAppDescription, actor); err != nil {
		if errors.Is(err, errSecretNameAmbiguous) {
			return connectReply(connectReasonSecretNameAmbiguous, ConnectTarget{})
		}
		return nil, err
	}
	call := renderCall("setGlobalVariable", map[string]any{
		"id": varID, "name": idName, "value": clientID, "description": pendingAppDescription, "active": true,
	})
	if _, err := c.engine.Execute(opCtx, call); err != nil {
		return nil, fmt.Errorf("shopify: write %s: %w", idName, err)
	}
	c.auditConnect(ctx, "shopify_app_saved", actor, target)
	return connectReply(connectReasonOK, target)
}

// variableRowID is the seeder's globalVariable id (scripts/secrets,
// component/memql/default_injector.go), so a value written here and one an
// operator seeded are the same row.
func variableRowID(name string) string {
	return "var-global-" + strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// ---------------------------------------------------------------------------
// shopifyConnectBegin (12.4)
// ---------------------------------------------------------------------------

// connectStateTTL bounds an in-flight Connect: GitHub Connect's ten minutes,
// inside the fifteen the consume accepts as a lifetime a server wrote.
const connectStateTTL = 10 * time.Minute

// Where the app a Connect state names comes from. The callback verifies
// Shopify's signature with the client secret this names, and only a "pending"
// one is promoted on approval (D12).
const (
	credentialSourcePending = "pending"
	credentialSourceCurrent = "current"
)

// handleConnectBegin answers {authorizeUrl, reason}: Shopify's approve page
// for the shop the server resolved, carrying a single-use state bound to the
// caller and to the browser session they called from (D16). The state is
// written through component/identity's Store, which stamps internal origin
// itself -- this handler stamps nothing, and writes as the person rather than
// as the deployment, because the row names them.
func (i *Integration) handleConnectBegin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	actor, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	target, reason, err := c.ResolveConnectSite(ctx, argString(args, "siteId"))
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return beginReply("", reason)
	}
	clientID, source, err := c.connectCredentials(ctx, target)
	if err != nil {
		return nil, err
	}
	if source == "" {
		return beginReply("", connectReasonShopifyAppNotSaved)
	}
	// The cluster's own identity service, composed as GitHub Connect composes
	// its redirect (envregistry derives it from MEMQL_DOMAIN on every node
	// type) and never from a request. A node that cannot say where that is
	// writes no state it could never finish.
	identityBase := strings.TrimRight(strings.TrimSpace(os.Getenv("MEMQL_IDENTITY_BASE_URL")), "/")
	if identityBase == "" {
		return nil, fmt.Errorf("shopify: this node has no MEMQL_IDENTITY_BASE_URL, so Connect cannot say where Shopify sends the browser back")
	}
	// D16: the state is bound to the browser session this call came from, as
	// githubConnectBegin binds GitHub Connect's, and the callback proves that
	// same live session from its refresh cookie. No session, no state. There is
	// no PKCE verifier: Shopify's authorization code grant takes none.
	claims, _ := auth.ClaimsFromContext(ctx)
	sessionID, _ := claims["sid"].(string)
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("shopify: Connect Shopify finishes in the browser that began it, and this call carries no browser session")
	}
	state, err := randomConnectState()
	if err != nil {
		return nil, fmt.Errorf("shopify: mint a Connect state: %w", err)
	}
	store := &componentIdentity.Store{Engine: c.engine, Logger: c.logger}
	if _, err := store.CreateGithubConnectState(ctx, componentIdentity.GithubConnectStateSeed{
		UserId:    actor,
		SessionId: strings.TrimSpace(sessionID),
		// Only the digest is stored: a row read is not a way to finish
		// somebody else's Connect.
		StateHash:        componentIdentity.HashConnectState(state),
		ReturnPath:       componentIdentity.SafeRelativeRedirect(argString(args, "returnPath")),
		ExpiresAt:        c.now().UTC().Add(connectStateTTL),
		Purpose:          githubconnect.PurposeShopifyConnect,
		ShopDomain:       target.ShopDomain,
		SiteID:           memql.BareShortId(target.SiteID),
		ClientID:         clientID,
		CredentialSource: source,
	}); err != nil {
		c.logger.Warn("shopify: Connect could not store its state row", "store", target.StoreID, "error", err.Error())
		return beginReply("", connectReasonStateInvalid)
	}
	redirectURI := identityBase + githubconnect.ShopifyCallbackPath
	return beginReply(connectAuthorizeURL(target.ShopDomain, clientID, redirectURI, state), connectReasonOK)
}

// connectCredentials is the app a Connect asks Shopify to approve: the pending
// one while a save waits for an approval, otherwise the store's current app
// with its webhook secret. Neither is source "".
func (c *Connector) connectCredentials(ctx context.Context, target ConnectTarget) (clientID, source string, err error) {
	pending, err := c.pendingClientID(ctx, target.StoreID)
	if err != nil {
		return "", "", err
	}
	if pending != "" {
		return pending, credentialSourcePending, nil
	}
	s := target.Store
	if !target.StoreFound || s.AppClientID == "" || s.WebhookSecretRef == "" {
		return "", "", nil
	}
	sealed, err := c.namedRowValue(ctx, conceptGlobalSecret, s.WebhookSecretRef, "encryptedValue")
	if err != nil || sealed == "" {
		return "", "", err
	}
	return s.AppClientID, credentialSourceCurrent, nil
}

// connectAuthorizeURL is Shopify's authorization-code grant, in 12.4's order
// with every value query-escaped. It sends no grant_options[], so the token is
// an offline one. shop is NormalizeShopDomain's, so it is a host and only that.
func connectAuthorizeURL(shop, clientID, redirectURI, state string) string {
	return "https://" + shop + "/admin/oauth/authorize" +
		"?client_id=" + url.QueryEscape(clientID) +
		"&scope=" + url.QueryEscape(strings.Join(ConnectScopes(), ",")) +
		"&redirect_uri=" + url.QueryEscape(redirectURI) +
		"&state=" + url.QueryEscape(state)
}

// randomConnectState is 32 random bytes, URL-safe -- GitHub Connect's state.
// A failure is the caller's error, never a guessable fallback.
func randomConnectState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// beginReply is the {authorizeUrl, reason} Begin answers, both keys always.
func beginReply(authorizeURL, reason string) ([]memorynodes.MemoryNode, error) {
	return resultNode("shopify", map[string]any{"authorizeUrl": authorizeURL, "reason": reason})
}

// ---------------------------------------------------------------------------
// shopifyStorefrontTokenSet (12.5)
// ---------------------------------------------------------------------------

const storefrontTokenDescription = "A Storefront API token pasted on a storefront's Store panel in MemQL OS."

func (i *Integration) handleStorefrontTokenSet(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	c := i.connector
	actor, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	target, reason, err := c.ResolveConnectSite(ctx, argString(args, "siteId"))
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return connectReply(reason, ConnectTarget{})
	}
	if !target.StoreFound {
		// A token needs a store row to point from, and a row made here would
		// be a store with no Admin token that every runner then picks up.
		return connectReply(connectReasonStoreNotConnected, ConnectTarget{})
	}

	sites, err := c.sitesBoundTo(ctx, target.StoreID)
	if err != nil {
		return nil, err
	}
	// A store's token is served under every site bound to it, so changing it
	// is an act on each of them.
	if ac, _ := auth.AccessFromContext(ctx); !ac.IsClusterOwner() {
		for _, site := range sites {
			ok, err := c.mayWriteRow(ctx, conceptPlatformSite, mapString(site, "id"))
			if err != nil {
				return nil, err
			}
			if !ok {
				return connectReply(connectReasonStoreInUse, ConnectTarget{})
			}
		}
	}

	opCtx := operatorContext(ctx)
	token := argString(args, "token")
	if token == "" {
		for _, site := range sites {
			binding, _ := rowValue(site, "binding").(map[string]any)
			if mapString(site, "status") == "live" && memql.BareShortId(mapString(binding, "storeId")) == target.StoreID {
				return connectReply(connectReasonStorefrontTokenRequired, ConnectTarget{})
			}
		}
		if err := c.setStorefrontTokenRef(opCtx, target.StoreID, ""); err != nil {
			return nil, err
		}
		c.auditConnect(ctx, "shopify_storefront_token_cleared", actor, target)
		return connectReply(connectReasonOK, target)
	}

	if !c.storefrontTokenWorks(ctx, target, token) {
		return connectReply(connectReasonStorefrontTokenInvalid, ConnectTarget{})
	}
	name := storeSecretName(target.StoreID, suffixStorefrontToken)
	if _, err := seedSecret(opCtx, c.engine, name, token, storefrontTokenDescription, actor); err != nil {
		if errors.Is(err, errSecretNameAmbiguous) {
			return connectReply(connectReasonSecretNameAmbiguous, ConnectTarget{})
		}
		return nil, err
	}
	if err := c.setStorefrontTokenRef(opCtx, target.StoreID, name); err != nil {
		return nil, err
	}
	c.auditConnect(ctx, "shopify_storefront_token_set", actor, target)
	return connectReply(connectReasonOK, target)
}

// sitesBoundTo is every site whose serving or preview binding names the store,
// read as the deployment so a site the caller cannot see is still counted.
func (c *Connector) sitesBoundTo(ctx context.Context, storeID string) ([]map[string]any, error) {
	ids := []string{storeID, "v1:shopify:store:" + storeID}
	res, err := c.engine.Execute(operatorContext(ctx), renderCall("sitesBoundToStore", map[string]any{"storeIds": ids}))
	if err != nil {
		return nil, fmt.Errorf("shopify: read the sites bound to %q: %w", storeID, err)
	}
	return memql.MaterializeRows(res), nil
}

func (c *Connector) setStorefrontTokenRef(ctx context.Context, storeID, ref string) error {
	call := renderCall("updateStore", map[string]any{"storeId": storeID, "storefrontTokenRef": ref})
	if _, err := c.engine.Execute(ctx, call); err != nil {
		return fmt.Errorf("shopify: point store %q at its Storefront token: %w", storeID, err)
	}
	c.stores.Invalidate()
	return nil
}

// storefrontTokenWorks asks the store's Storefront API one question with the
// token, in a header and never in the URL. The answer is yes or no, and the
// log line names the store alone: storefrontCall's sentences can carry
// Shopify's own error text, which is part of a response body.
func (c *Connector) storefrontTokenWorks(ctx context.Context, target ConnectTarget, token string) bool {
	endpoint := c.storefrontEndpoint
	if endpoint == nil {
		endpoint = StorefrontEndpoint
	}
	store := storeWithVersion(target.Store)
	url, err := endpoint(store.Domain, store.APIVersion)
	if err == nil {
		var out struct {
			Shop struct {
				Name string `json:"name"`
			} `json:"shop"`
		}
		err = storefrontCall(ctx, url, token, `{ shop { name } }`, nil, &out)
	}
	if err != nil {
		c.logger.Info("shopify: a pasted Storefront token did not answer the store's Storefront API", "store", target.StoreID)
		return false
	}
	return true
}

// ---------------------------------------------------------------------------

// connectReply is the {reason, storeId, shopDomain} a write answers. A refusal
// names no store.
func connectReply(reason string, target ConnectTarget) ([]memorynodes.MemoryNode, error) {
	return resultNode("shopify", map[string]any{
		"reason": reason, "storeId": target.StoreID, "shopDomain": target.ShopDomain,
	})
}

// auditConnect records one Connect act by a person: category configuration,
// target the store, and no value, token or fingerprint. A failed audit write is
// loud but does not undo the act, for compliance.go's reason.
func (c *Connector) auditConnect(ctx context.Context, action, actor string, target ConnectTarget) {
	at := c.now().UTC()
	call := renderCall("createAuditEvent", map[string]any{
		"eventId":     "aud" + MirrorRowID(target.StoreID, action+"\x00"+actor+"\x00"+at.Format(time.RFC3339Nano)),
		"occurredAt":  at.Format(time.RFC3339),
		"category":    "configuration",
		"action":      action,
		"actorUserId": actor,
		"targetType":  "shopifyStore",
		"targetId":    target.StoreID,
		"detail":      map[string]any{"siteId": memql.BareShortId(target.SiteID), "shopDomain": target.ShopDomain},
		"outcome":     "success",
	})
	if _, err := c.engine.Execute(operatorContext(ctx), call); err != nil {
		c.logger.Error("shopify: could not write a Connect audit event", "action", action, "store", target.StoreID, "error", err)
	}
}

// connectCapabilities is the Store panel's surface, appended to Capabilities().
// Each is declared with @requiresCapability("execute", "app:deployables/store")
// in dsl/shopify/overlay/builtins.memql, which the engine asks before the
// handler runs.
func (i *Integration) connectCapabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "connectStatus",
			Description: "Where one storefront stands with Connect Shopify, from the store the server resolves for the site. Returns {reason, storeId, shopDomain, appSaved, pendingApp, connected, storefrontTokenSet, requiredScopes, grantedScopes}. Never returns a credential.",
			Handler:     i.handleConnectStatus,
			ArgsSchema:  map[string]string{"siteId": "string - the storefront deployable"},
		},
		{
			Name:        "storeAppSave",
			Description: "Save a Shopify app's client ID and secret for a storefront's store as PENDING credentials, sealed server-side; the store row and its live credentials are untouched. Returns {reason, storeId, shopDomain}.",
			Handler:     i.handleStoreAppSave,
			ArgsSchema: map[string]string{
				"siteId":       "string - the storefront deployable",
				"clientId":     "string - the app's client ID",
				"clientSecret": "string - the app's client secret, sealed and never echoed",
			},
		},
		{
			Name:        "connectBegin",
			Description: "Begin Connect Shopify for a storefront's store: write a ten-minute single-use state bound to the caller and answer {authorizeUrl, reason}, Shopify's approve page for the shop the server resolved. Asks Shopify to approve the pending app when one was saved, otherwise the store's current app.",
			Handler:     i.handleConnectBegin,
			ArgsSchema: map[string]string{
				"siteId":     "string - the storefront deployable",
				"returnPath": "string (optional) - the same-origin OS path to land on afterwards",
			},
		},
		{
			Name:        "storefrontTokenSet",
			Description: "Check and seal a pasted Storefront API token for a storefront's store, or clear the store's token with an empty one. Returns {reason, storeId, shopDomain}.",
			Handler:     i.handleStorefrontTokenSet,
			ArgsSchema: map[string]string{
				"siteId": "string - the storefront deployable",
				"token":  "string (optional) - the Storefront token; empty clears it",
			},
		},
	}
}
