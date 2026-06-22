package telephony

import "testing"

// TestNewLoadsWithoutCarrierKey asserts the telephony integration constructs
// successfully when no carrier API key is configured. A missing key must NOT
// fail plug-in load -- the plug-in registers on every node, telephony or not,
// and a fatal-at-boot here took staging fully down once (#2028). The error must
// surface only when a telephony capability is actually invoked.
func TestNewLoadsWithoutCarrierKey(t *testing.T) {
	t.Setenv("MEMQL_TELEPHONY_TELNYX_API_KEY", "")

	integ, err := New(Config{}, nil)
	if err != nil {
		t.Fatalf("New() must not fail without a carrier key; got: %v", err)
	}
	if integ == nil {
		t.Fatal("New() returned a nil integration")
	}
	if integ.carrier != nil {
		t.Fatal("expected carrier to be nil when no key is configured")
	}

	// A capability that needs the carrier must return a clear error, not panic.
	if _, err := integ.requireCarrier(); err == nil {
		t.Fatal("requireCarrier() must error when the carrier is not configured")
	}
}
