package router

import "testing"

// The router integration seals no key (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decision K18). It had a capability that encrypted a vendor key and stored it
// as a secret no call ever read. A person's API key is added through
// integrations/vendorkeys, as that person's own row, and the cluster has no key
// of its own.
func TestTheRouterIntegrationSealsNoKey(t *testing.T) {
	for _, capability := range New(nil, nil, nil, nil).Capabilities() {
		if capability.Name == "setApiKey" {
			t.Fatalf("the router integration still offers %q: %s", capability.Name, capability.Description)
		}
	}
}
