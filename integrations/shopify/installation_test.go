package shopify

import (
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestShopifyInstallationRequiresRecentSignedLaunch(t *testing.T) {
	now := time.Now().UTC()
	base := map[string]string{"shop": "client-store.myshopify.com", "timestamp": strconv.FormatInt(now.Unix(), 10), "host": "shopify-host"}
	good := signedQuery("secret", base).Encode()
	shop, ok := verifiedShopifyLaunch(good, "secret", now)
	if !ok || shop != base["shop"] {
		t.Fatal("signed Shopify launch rejected")
	}
	for name, raw := range map[string]string{
		"unsigned":       "shop=client-store.myshopify.com",
		"forged":         signedQuery("other-secret", base).Encode(),
		"duplicate shop": good + "&shop=other.myshopify.com",
		"callback":       signedQuery("secret", map[string]string{"shop": base["shop"], "timestamp": base["timestamp"], "code": "callback-code"}).Encode(),
		"old":            signedQuery("secret", map[string]string{"shop": base["shop"], "timestamp": strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10)}).Encode(),
		"future":         signedQuery("secret", map[string]string{"shop": base["shop"], "timestamp": strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10)}).Encode(),
		"invalid host":   signedQuery("secret", map[string]string{"shop": "evil.example", "timestamp": base["timestamp"]}).Encode(),
		"tampered":       good + "&extra=" + url.QueryEscape("not signed"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := verifiedShopifyLaunch(raw, "secret", now); ok {
				t.Fatal("untrusted launch admitted")
			}
		})
	}
}

func TestShopifyInstallationDestinationIsShopifyOwned(t *testing.T) {
	for _, raw := range []string{"https://apps.shopify.com/memql", "https://admin.shopify.com/?organization_id=123&no_redirect=true&redirect=%2Foauth%2Fredirect_from_developer_dashboard%3Fclient_id%3Dclient"} {
		if !validManagedInstallURL(raw, "client") {
			t.Fatalf("valid destination rejected: %s", raw)
		}
	}
	for _, raw := range []string{"", "http://apps.shopify.com/memql", "https://apps.shopify.com.evil.test/memql", "https://apps.shopify.com@evil.test/memql", "https://user@apps.shopify.com/memql", "https://apps.shopify.com/memql?redirect=https://evil.test", "https://dev.shopify.com/", "https://apps.shopify.com:8443/memql", "https://admin.shopify.com/?organization_id=123&no_redirect=true&redirect=https%3A%2F%2Fevil.test", "https://admin.shopify.com/?organization_id=123&no_redirect=true&redirect=%2Foauth%2Fredirect_from_developer_dashboard%3Fclient_id%3Dother"} {
		if validManagedInstallURL(raw, "client") {
			t.Fatalf("unsafe destination admitted: %s", raw)
		}
	}
}
