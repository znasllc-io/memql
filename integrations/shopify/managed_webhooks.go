package shopify

import (
	"context"
	"strings"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

func shopifyDeliveryHeaders() []string {
	return []string{HeaderTopic, HeaderShopDomain, HeaderWebhookID, HeaderEventID, HeaderTriggeredAt, HeaderAPIVersion}
}

// managedAppSigning is the ONE predicate for "this cluster's managed app can
// verify an app-level delivery": the client id AND the sealed secret both
// resolve. It is what InboundSource(ConnectorName) offers the receiver and
// what StoreFor requires of a row staged under the app-level source, so a
// half-configured app -- a client id with no sealed or sealable secret --
// claims nothing on either side. Before this was one predicate, the receiver
// asked "client and secret" while the connector bound stores by client id
// alone, and a row staged under `shopify` by any other verifier (an env pin,
// a node with no connector bound) was bound to a managed store as if the app
// had signed it (memql#5707 review).
func (c *Connector) managedAppSigning(ctx context.Context) (client, secret string, ok bool) {
	client, secret, err := c.managedApp(ctx)
	if err != nil || client == "" || secret == "" {
		return "", "", false
	}
	return client, secret, true
}

// managedInboundSource is the app-level endpoint required for the three
// mandatory privacy subscriptions. It works before the first store connects
// and after uninstall; an install-listing URL is not needed to verify HMAC.
func (c *Connector) managedInboundSource(ctx context.Context) (memqlsync.InboundSource, bool) {
	client, secret, ok := c.managedAppSigning(ctx)
	if !ok || client == "" {
		return memqlsync.InboundSource{}, false
	}
	return memqlsync.InboundSource{
		Name: ConnectorName, Scheme: "hmac-sha256-base64",
		SignatureHeader: HeaderHMAC, DedupeHeader: HeaderWebhookID,
		ForwardHeaders: shopifyDeliveryHeaders(),
		Secret:         secret, SecretRef: managedClientSecret,
	}, true
}

// managedComplianceStore uses the SIGNED shop_domain, never the unsigned
// shop-domain header. The app-level endpoint may only touch this app's stores,
// and only while the app can actually have signed the delivery
// (managedAppSigning); Apply refuses the row with a reason before this runs.
func (c *Connector) managedComplianceStore(ctx context.Context, req memqlsync.InboundRequest) (Store, bool) {
	client, _, ok := c.managedAppSigning(ctx)
	if !ok {
		return Store{}, false
	}
	topic := req.Topic
	if topic == "" {
		topic = header(req, HeaderTopic)
	}
	if !isComplianceTopic(topic) {
		return Store{}, false
	}
	obj, err := decodeJSONObject(string(req.Body))
	if err != nil {
		return Store{}, false
	}
	domain, err := NormalizeShopDomain(firstString(obj, "shop_domain"))
	if err != nil {
		return Store{}, false
	}
	if h := header(req, HeaderShopDomain); h != "" && !strings.EqualFold(h, domain) {
		return Store{}, false
	}
	store, ok := c.stores.ByDomain(ctx, domain)
	if !ok {
		return Store{}, false
	}
	return store, store.AppClientID == client
}

// storeSignsWithManagedSecret says whether the store's per-store webhook
// secret is the managed app's. Connect Shopify seals a COPY of the app client
// secret as the webhook secret of every store it installs (connect_write.go),
// so that store's per-store source and the app-level source verify the same
// signature. Deliberately keyed on the client id alone, unlike
// managedAppSigning: the question is whether the STORE was installed by the
// managed app, and that stays true -- the sealed copy keeps verifying -- when
// the app's own secret row is later missing.
func (c *Connector) storeSignsWithManagedSecret(ctx context.Context, store Store) bool {
	if store.AppClientID == "" {
		return false
	}
	client, _, err := c.managedApp(ctx)
	return err == nil && client != "" && store.AppClientID == client
}
