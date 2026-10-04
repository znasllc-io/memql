package shopify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// uninstall.go -- app/uninstalled, and what disconnecting a store means
// (memql#5638, G1).
//
// Shopify sends app/uninstalled when a shop's staff remove the app, and from
// that moment every token the app holds for the shop is dead. Before this the
// topic was dropped as unmirrored, so a store kept its Admin grant reference,
// the edge kept serving its storefronts as `connected` with a token Shopify
// refuses, and the person's saved connection kept reading Connected.
//
// # What a delivery does
//
// It is declared on the APP-LEVEL endpoint, beside the privacy topics, and
// bound the way they are: the app's secret verified it, and the store is the
// one the SIGNED body names (`myshopify_domain`) and this app installed
// (managedAppLevelStore). Then the store is disconnected:
//
//   - uninstalledAt is stamped, which is what ingestion (Store.Ingests) and the
//     edge's storefront block read;
//   - the Admin grant reference is dropped from the row and the sealed grant
//     blanked -- its refresh token is dead too;
//   - the store owner's saved connection to it reads `disconnected`;
//   - the act is audited.
//
// The mirror stays. Purging is shop/redact's job, 48 hours later, after its
// own check that the store did not come back.
//
// # A delivery is a trigger, and the grant is the truth
//
// The connector's first rule (apply.go) holds here too. Shopify retries a
// delivery it could not hand over for four hours, so an app/uninstalled can
// arrive after the shop has REINSTALLED and Connect Shopify has sealed a fresh
// grant. Disconnecting then would undo a working connection. So the store's
// current grant is asked one question first, and a grant that answers means
// the uninstall was superseded: nothing changes, and that is audited too. A
// grant that does not answer -- refused, or unreachable -- leaves the signed
// delivery's word standing.
//
// The judgement and the disconnect run under the store's offline lock, the one
// a reinstall's whole write and every refresh hold (connections.go,
// offline_token.go), so no reinstall lands between the question and the act.

// TopicAppUninstalled is the topic header Shopify delivers app/uninstalled
// under. Not a generated topic: it is the app's, declared in its configuration
// on the app-level endpoint, and it mirrors nothing.
const TopicAppUninstalled = "app/uninstalled"

func isUninstallTopic(topic string) bool {
	return strings.EqualFold(strings.TrimSpace(topic), TopicAppUninstalled)
}

// connectionStatusDisconnected is what a saved connection reads once its store
// is uninstalled: not `removed`, because the person removed nothing, and not
// `active`, which is what every surface draws as Connected.
const connectionStatusDisconnected = "disconnected"

const grantClearedDescription = "Cleared by the Shopify connector: the shop uninstalled the app, which ended this grant."

// applyUninstall disconnects a store Shopify reported the app uninstalled
// from. Every write is idempotent on the store, so a delivery worked twice
// changes nothing the first did not.
func (c *Connector) applyUninstall(ctx context.Context, store Store, req memqlsync.InboundRequest) error {
	obj, err := decodeJSONObject(string(req.Body))
	if err != nil {
		return fmt.Errorf("shopify: %s delivery is not JSON", TopicAppUninstalled)
	}
	// managedAppLevelStore bound the store by this same field; asked again
	// here, as parseComplianceJob asks it, so the handler does not depend on
	// which binder reached it.
	if domain := firstString(obj, "myshopify_domain"); domain == "" || !strings.EqualFold(domain, store.Domain) {
		return fmt.Errorf("shopify: %s delivery shop does not match the store", TopicAppUninstalled)
	}
	unlock, err := c.acquireConnectApp(ctx, "offline:"+store.ID)
	if err != nil {
		return fmt.Errorf("shopify: %s for store %s: connection storage is unavailable", TopicAppUninstalled, store.ID)
	}
	defer unlock()
	defer c.stores.Invalidate()

	// Read FRESH, by id, as the deployment. Neither the registry's 30-second
	// cache nor this node's result cache may answer: a reinstall another
	// replica committed under this same lock a moment ago is evicted here only
	// when its broadcast arrives, and judging the grant it replaced would
	// disconnect the store it just reconnected (memql#5431's read-modify-write
	// rule). The marked context stays a local.
	fresh := memql.ContextWithFreshRead(ctx)
	opCtx := operatorContext(ctx)
	current, found, err := c.storeByID(operatorContext(fresh), store.ID)
	if err != nil {
		return fmt.Errorf("shopify: %s for store %s: read the store: %w", TopicAppUninstalled, store.ID, err)
	}
	if !found {
		return nil
	}
	at := deliveredAt(req, c.now())
	if current.AdminTokenRef != "" && c.grantAnswers(fresh, current) {
		c.logger.Info("shopify: app/uninstalled arrived for a store whose current grant still answers; a reinstall superseded it", "store", current.ID)
		c.auditUninstall(ctx, current, "shopify_app_uninstall_superseded", at, req, nil)
		return nil
	}

	if _, err := c.engine.Execute(opCtx, renderCall("markStoreUninstalled", map[string]any{
		"storeId": current.ID, "uninstalledAt": at.Format(time.RFC3339),
	})); err != nil {
		return fmt.Errorf("shopify: %s for store %s: mark the store disconnected: %w", TopicAppUninstalled, current.ID, err)
	}
	var problems []string
	if err := c.clearDeadGrant(opCtx, current); err != nil {
		c.logger.Warn("shopify: the store is disconnected and its dead grant could not be blanked", "store", current.ID, "error", err.Error())
		problems = append(problems, "the sealed grant could not be blanked")
	}
	if err := c.disconnectPersonalConnections(ctx, current); err != nil {
		c.logger.Error("shopify: the store is disconnected and its owner's saved connection still reads connected", "store", current.ID, "error", err.Error())
		problems = append(problems, "the owner's saved connection could not be marked disconnected")
	}
	c.auditUninstall(ctx, current, "shopify_app_uninstalled", at, req, problems)
	if len(problems) > 0 {
		// The store itself is disconnected, which is what serving and
		// ingestion read; the delivery is stamped failed so an operator sees
		// what is left over.
		return fmt.Errorf("shopify: %s for store %s: the store is disconnected, but %s", TopicAppUninstalled, current.ID, strings.Join(problems, " and "))
	}
	return nil
}

// grantAnswers reports whether the store's current Admin grant still works:
// one trivial read, refreshing an expiring offline grant the way every call
// does. The caller holds the store's offline lock, so the grant is read through
// offlineAdminTokenLocked rather than adminToken.
func (c *Connector) grantAnswers(ctx context.Context, store Store) bool {
	var token string
	var err error
	if strings.HasSuffix(store.AdminTokenRef, "_OFFLINE_GRANT") {
		token, err = c.offlineAdminTokenLocked(ctx, store)
	} else {
		token, err = c.stores.AdminToken(ctx, store)
	}
	if err != nil || token == "" {
		return false
	}
	_, err = c.admin.Do(ctx, storeWithVersion(store), token, `query ShopifyReachable { shop { id } }`, "ShopifyReachable", nil)
	return err == nil
}

// clearDeadGrant blanks the sealed grant the store named, as clearPendingApp
// blanks a promoted pending pair: the row stays, carrying no value. Only a name
// this connector seals for the store is blanked -- an operator's hand-set
// reference to some other secret is not this connector's to empty.
func (c *Connector) clearDeadGrant(ctx context.Context, store Store) error {
	name := store.AdminTokenRef
	if name != storeSecretName(store.ID, "OFFLINE_GRANT") && name != storeSecretName(store.ID, suffixAdminToken) {
		return nil
	}
	id, err := namedRowID(ctx, c.engine, conceptGlobalSecret, name, secretRowID(name))
	if err != nil {
		return err
	}
	_, err = c.engine.Execute(ctx, renderCall("setGlobalSecret", map[string]any{
		"id": id, "name": name, "encryptedValue": "", "fingerprint": "", "kind": "vendor_api_key",
		"description": grantClearedDescription, "addedBy": "system:connector:" + ConnectorName, "active": false,
	}))
	return err
}

// disconnectPersonalConnections marks the store owner's saved Shopify
// connections to this store `disconnected`, so Settings -> Connections and the
// storefront's store picker stop offering it as connected. A reinstall's
// savePersonalConnection writes the same row back to active.
//
// The OWNER's alone, read under their borrowed identity: a saved connection is
// owner-tier and readable by nobody else, and Connect Shopify lets only the
// store's owner connect a store that exists (managedTarget), so theirs is the
// one there is.
func (c *Connector) disconnectPersonalConnections(ctx context.Context, store Store) error {
	owner := strings.TrimSpace(store.OwnerUserID)
	if owner == "" {
		return nil
	}
	person := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner))
	res, err := c.engine.Execute(person, "query externalConnectionsMine()")
	if err != nil {
		return fmt.Errorf("read the owner's saved connections: %w", err)
	}
	for _, row := range memql.MaterializeRows(res) {
		if mapString(row, "provider") != ConnectorName || memql.BareShortId(mapString(row, "resourceId")) != store.ID || mapString(row, "status") != "active" {
			continue
		}
		call := renderCall("markExternalConnectionDisconnected", map[string]any{"connectionId": mapString(row, "id")})
		if _, err := c.engine.Execute(person, call); err != nil {
			return fmt.Errorf("mark a saved connection disconnected: %w", err)
		}
	}
	return nil
}

// auditUninstall records what an app/uninstalled did. The actor is the
// connector: the shop's staff acted at Shopify, and the delivery is all of it
// this cluster saw.
func (c *Connector) auditUninstall(ctx context.Context, store Store, action string, at time.Time, req memqlsync.InboundRequest, problems []string) {
	detail := map[string]any{
		"topic":         TopicAppUninstalled,
		"shopDomain":    store.Domain,
		"uninstalledAt": at.Format(time.RFC3339),
	}
	if id := header(req, HeaderWebhookID); id != "" {
		detail["webhookId"] = id
	}
	outcome := "success"
	if len(problems) > 0 {
		detail["problems"] = toAny(problems)
		outcome = "failure"
	}
	call := renderCall("createAuditEvent", map[string]any{
		"eventId":     "aud" + MirrorRowID(store.ID, action+"\x00"+req.RequestId+"\x00"+at.Format(time.RFC3339)),
		"occurredAt":  c.now().UTC().Format(time.RFC3339),
		"category":    "configuration",
		"action":      action,
		"actorUserId": "system:connector:" + ConnectorName,
		"targetType":  "shopifyStore",
		"targetId":    store.ID,
		"detail":      detail,
		"outcome":     outcome,
	})
	if _, err := c.engine.Execute(operatorContext(ctx), call); err != nil {
		c.logger.Error("shopify: could not write an app/uninstalled audit event", "action", action, "store", store.ID, "error", err)
	}
}
