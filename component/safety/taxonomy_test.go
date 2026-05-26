package safety

import "testing"

func TestRiskTierOrdering(t *testing.T) {
	// Tiers must be strictly ordered low→high so the decision policy
	// can compare with `>=` -- a swap here silently broadens or
	// narrows enforcement.
	tiers := []RiskTier{TierNone, TierLow, TierMedium, TierHigh, TierCritical}
	for i := 1; i < len(tiers); i++ {
		if !(tiers[i-1] < tiers[i]) {
			t.Errorf("tier %s should sort before %s", tiers[i-1], tiers[i])
		}
	}
}

func TestRiskTierStringRoundTrip(t *testing.T) {
	for _, tier := range []RiskTier{TierNone, TierLow, TierMedium, TierHigh, TierCritical} {
		parsed, ok := ParseRiskTier(tier.String())
		if !ok || parsed != tier {
			t.Errorf("round-trip %s: parsed=%v ok=%v", tier, parsed, ok)
		}
	}
}

func TestParseRiskTierUnknown(t *testing.T) {
	if _, ok := ParseRiskTier("severe"); ok {
		t.Error("unknown label should not parse")
	}
	if _, ok := ParseRiskTier(""); ok {
		t.Error("empty should not parse")
	}
}
