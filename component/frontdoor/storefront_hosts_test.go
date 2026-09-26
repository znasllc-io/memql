package frontdoor

import (
	"strings"
	"testing"
)

func TestStorefrontDestinationHosts(t *testing.T) {
	host := StorefrontTestingHost("  graceful-fjord.memql.znas.io ")
	if host != "test--graceful-fjord.memql.znas.io" {
		t.Fatal(host)
	}
	if got, ok := StorefrontProductionHost(host); !ok || got != "graceful-fjord.memql.znas.io" {
		t.Fatal(got, ok)
	}
	for _, bad := range []string{"", "shop", "test--shop.example.com", strings.Repeat("a", 58) + ".example.com"} {
		if StorefrontTestingHost(bad) != "" {
			t.Fatalf("accepted %q", bad)
		}
	}
}
