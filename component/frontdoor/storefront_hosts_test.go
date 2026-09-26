package frontdoor

import (
	"strings"
	"testing"
)

func TestStorefrontDestinationHosts(t *testing.T) {
	host := StorefrontTestingHost("  graceful-fjord.cluster.example.com ")
	if host != "test--graceful-fjord.cluster.example.com" {
		t.Fatal(host)
	}
	if got, ok := StorefrontProductionHost(host); !ok || got != "graceful-fjord.cluster.example.com" {
		t.Fatal(got, ok)
	}
	for _, bad := range []string{"", "shop", "test--shop.example.com", strings.Repeat("a", 58) + ".example.com"} {
		if StorefrontTestingHost(bad) != "" {
			t.Fatalf("accepted %q", bad)
		}
	}
}
