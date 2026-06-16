package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthRejectIncrements(t *testing.T) {
	before := AuthRejectValue(SurfaceNode, ReasonUnknownKID, CodeUnauthenticated)

	AuthReject(SurfaceNode, ReasonUnknownKID, CodeUnauthenticated)
	AuthReject(SurfaceNode, ReasonUnknownKID, CodeUnauthenticated)

	got := AuthRejectValue(SurfaceNode, ReasonUnknownKID, CodeUnauthenticated)
	if got-before != 2 {
		t.Fatalf("auth reject counter delta = %v, want 2", got-before)
	}

	// A different label set must be tracked independently.
	other := AuthRejectValue(SurfaceGRPC, ReasonInvalidToken, CodeUnauthenticated)
	AuthReject(SurfaceGRPC, ReasonInvalidToken, CodeUnauthenticated)
	if AuthRejectValue(SurfaceGRPC, ReasonInvalidToken, CodeUnauthenticated)-other != 1 {
		t.Fatalf("independent label set did not increment by 1")
	}
	if AuthRejectValue(SurfaceNode, ReasonUnknownKID, CodeUnauthenticated) != got {
		t.Fatalf("incrementing one label set bled into another")
	}
}

func TestKeysetFingerprintDeterministicAndDistinct(t *testing.T) {
	// Order-independent: same set, different order -> same fingerprint.
	a := KeysetFingerprint([]string{"k1", "k2", "k3"})
	b := KeysetFingerprint([]string{"k3", "k1", "k2"})
	if a != b {
		t.Fatalf("fingerprint not order-independent: %v != %v", a, b)
	}

	// Different sets -> different fingerprints (the incoherence signal).
	c := KeysetFingerprint([]string{"k1", "k2"})
	if a == c {
		t.Fatalf("distinct key sets produced identical fingerprint %v", a)
	}

	// Empty set is the sentinel zero.
	if KeysetFingerprint(nil) != 0 {
		t.Fatalf("empty keyset fingerprint = %v, want 0", KeysetFingerprint(nil))
	}

	// The delimiter prevents concatenation collisions: {"ab","c"} != {"a","bc"}.
	if KeysetFingerprint([]string{"ab", "c"}) == KeysetFingerprint([]string{"a", "bc"}) {
		t.Fatalf("delimiter failed to prevent concatenation collision")
	}
}

func TestSetJWKSKeysetUpdatesGauges(t *testing.T) {
	SetJWKSKeyset([]string{"alpha", "beta"})
	if v := readGauge(t, "memql_jwks_keyset_keys"); v != 2 {
		t.Fatalf("keyset_keys gauge = %v, want 2", v)
	}
	wantFP := KeysetFingerprint([]string{"alpha", "beta"})
	if v := readGauge(t, "memql_jwks_keyset_fingerprint"); v != wantFP {
		t.Fatalf("keyset_fingerprint gauge = %v, want %v", v, wantFP)
	}
}

func TestHandlerExposesMetrics(t *testing.T) {
	AuthReject(SurfaceHTTP, ReasonMissingToken, CodeUnauthenticated)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	text := string(body)
	for _, want := range []string{
		"memql_auth_rejects_total",
		"memql_jwks_keyset_keys",
		"memql_jwks_keyset_fingerprint",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/metrics output missing %q", want)
		}
	}
}

// readGauge reads a single-series gauge value out of the registry by name.
func readGauge(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		m := mf.GetMetric()
		if len(m) != 1 {
			t.Fatalf("%s has %d series, want 1", name, len(m))
		}
		return m[0].GetGauge().GetValue()
	}
	t.Fatalf("gauge %q not found", name)
	return 0
}
