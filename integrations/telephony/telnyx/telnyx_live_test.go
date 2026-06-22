//go:build telnyx_live

// Live Telnyx API integration test. Excluded from the default build; run
// against the real API with credentials:
//
//	MEMQL_TELEPHONY_TELNYX_API_KEY=... go test -tags telnyx_live -run TestLive ./integrations/telephony/telnyx
//
// It only exercises read-only search + list (no purchase) so it is safe to
// run without spending money. A full buy/configure/release round-trip is an
// owner-driven staging step (it provisions a real DID).
package telnyx

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/znasllc-io/memql/integrations/telephony"
)

func liveClient(t *testing.T) *Client {
	t.Helper()
	key := os.Getenv("MEMQL_TELEPHONY_TELNYX_API_KEY")
	if key == "" {
		t.Skip("MEMQL_TELEPHONY_TELNYX_API_KEY not set; skipping live Telnyx test")
	}
	c, err := New(Options{APIKey: key, ConnectionID: os.Getenv("MEMQL_TELEPHONY_TELNYX_CONNECTION_ID")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestLiveSearchNumbers(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nums, err := c.SearchNumbers(ctx, telephony.NumberQuery{AreaCode: "415", Limit: 3})
	if err != nil {
		t.Fatalf("SearchNumbers: %v", err)
	}
	t.Logf("found %d available numbers", len(nums))
	for _, n := range nums {
		if n.E164 == "" {
			t.Errorf("empty E.164 in %+v", n)
		}
	}
}

func TestLiveListOwnedNumbers(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nums, err := c.ListNumbers(ctx)
	if err != nil {
		t.Fatalf("ListNumbers: %v", err)
	}
	t.Logf("account owns %d numbers", len(nums))
}
