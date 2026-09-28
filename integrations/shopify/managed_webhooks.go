package shopify

import (
	"context"
	"strings"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

func shopifyDeliveryHeaders() []string {
	return []string{HeaderTopic, HeaderShopDomain, HeaderWebhookID, HeaderEventID, HeaderTriggeredAt, HeaderAPIVersion}
}

// managedInboundSource is the app-level endpoint required for the three
// mandatory privacy subscriptions. It works before the first store connects
// and after uninstall; an install-listing URL is not needed to verify HMAC.
func (c *Connector) managedInboundSource(ctx context.Context) (memqlsync.InboundSource, bool) {
	client, secret, err := c.managedApp(ctx)
	if err != nil || client == "" || secret == "" {
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
// shop-domain header. The app-level endpoint may only touch this app's stores.
func (c *Connector) managedComplianceStore(ctx context.Context, req memqlsync.InboundRequest) (Store, bool) {
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
	client, _, err := c.managedApp(ctx)
	return store, err == nil && client != "" && store.AppClientID == client
}

// storeSignsWithManagedSecret says whether the store's per-store webhook
// secret is the managed app's. Connect Shopify seals a COPY of the app client
// secret as the webhook secret of every store it installs (connect_write.go),
// so that store's per-store source and the app-level source verify the same
// signature.
func (c *Connector) storeSignsWithManagedSecret(ctx context.Context, store Store) bool {
	if store.AppClientID == "" {
		return false
	}
	client, _, err := c.managedApp(ctx)
	return err == nil && client != "" && store.AppClientID == client
}
