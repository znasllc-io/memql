package main

import (
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

// A discovery document that names the cockpit's own client -- or none --
// must produce an entry with no client_id: clusters.yaml is shared with
// other tools and the default is compiled in. A different client is the
// cluster's explicit instruction to the cockpit and is kept.
func TestClusterFromDiscoveryStoresOnlyANonDefaultClient(t *testing.T) {
	for _, tc := range []struct {
		discovered string
		stored     string
		effective  string
	}{
		{discovered: "", stored: "", effective: "cockpit"},
		{discovered: "cockpit", stored: "", effective: "cockpit"},
		{discovered: "operator-client", stored: "operator-client", effective: "operator-client"},
	} {
		got := clusterFromDiscovery(&config.DiscoveryDocument{
			IdentityURL:  "https://identity.staging.example.com",
			GRPCEndpoint: "https://api.staging.example.com",
			ClientId:     tc.discovered,
		}, "staging.example.com")
		if got.ClientId != tc.stored {
			t.Errorf("discovery clientId %q: stored ClientId = %q, want %q", tc.discovered, got.ClientId, tc.stored)
		}
		if id := got.EffectiveClientId(); id != tc.effective {
			t.Errorf("discovery clientId %q: EffectiveClientId() = %q, want %q", tc.discovered, id, tc.effective)
		}
	}
}

func TestClusterAuthLabelIsOIDCWithoutAStoredClient(t *testing.T) {
	c := config.ComposeFromDomain("staging.example.com")
	if got := clusterAuthLabel(c); got != "OIDC" {
		t.Errorf("clusterAuthLabel(composed entry) = %q, want OIDC", got)
	}
}
