package telephony

import (
	"context"
	"testing"
)

// TestProvisioningDeniedWithoutPrivilege verifies the owner/admin gate: an
// unauthenticated / non-privileged context is rejected before any carrier call.
func TestProvisioningDeniedWithoutPrivilege(t *testing.T) {
	i := &Integration{}
	ctx := context.Background() // no claims -> not owner/admin
	cases := []struct {
		name string
		fn   func() error
	}{
		{"searchNumbers", func() error { _, err := i.handleSearchNumbers(ctx, map[string]any{}, 0); return err }},
		{"buyNumber", func() error {
			_, err := i.handleBuyNumber(ctx, map[string]any{"providerId": "+1", "partitionId": "p"}, 0)
			return err
		}},
		{"configureInboundNumber", func() error {
			_, err := i.handleConfigureInbound(ctx, map[string]any{"e164": "+1", "partitionId": "p"}, 0)
			return err
		}},
		{"releaseNumber", func() error { _, err := i.handleReleaseNumber(ctx, map[string]any{"e164": "+1"}, 0); return err }},
	}
	for _, c := range cases {
		if err := c.fn(); err == nil {
			t.Errorf("%s: expected owner/admin denial, got nil", c.name)
		}
	}
}

func TestAsInt(t *testing.T) {
	if asInt(float64(5)) != 5 || asInt("7") != 7 || asInt(3) != 3 || asInt(nil) != 0 {
		t.Fatal("asInt coercion wrong")
	}
}
