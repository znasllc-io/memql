package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
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

// TestSetIdentitySigningKey covers the honest-unknown contract (memql#3381).
// The gauge exists because signing-key rotation cannot happen from inside a
// deployed cluster, so an operator's only pressure to run the manual runbook
// is an alert on key age. A gauge that reported process start as the key's
// birthday would reset to "0 days old" on every pod restart -- healthy-looking
// precisely where rotation never happens -- so unknown must read as unknown.
func TestSetIdentitySigningKey(t *testing.T) {
	minted := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	t.Run("known age in env-seed mode", func(t *testing.T) {
		SetIdentitySigningKey(minted, true, false)
		created, known, rotates := IdentitySigningKeyValues()
		if created != float64(minted.Unix()) {
			t.Fatalf("created = %v, want %v", created, float64(minted.Unix()))
		}
		if known != 1 {
			t.Fatalf("age_known = %v, want 1", known)
		}
		if rotates != 0 {
			t.Fatalf("rotation_supported = %v, want 0", rotates)
		}
	})

	t.Run("unknown age publishes zero, not a timestamp", func(t *testing.T) {
		// A caller with a boot-time fallback in hand must not be able to
		// leak it into the gauge by passing createdAtKnown=false.
		SetIdentitySigningKey(time.Now(), false, false)
		created, known, _ := IdentitySigningKeyValues()
		if known != 0 {
			t.Fatalf("age_known = %v, want 0", known)
		}
		if created != 0 {
			t.Fatalf("created = %v, want 0 when the date is not known", created)
		}
	})

	t.Run("zero time is unknown even when claimed known", func(t *testing.T) {
		SetIdentitySigningKey(time.Time{}, true, true)
		created, known, rotates := IdentitySigningKeyValues()
		if known != 0 {
			t.Fatalf("age_known = %v, want 0 for the zero time", known)
		}
		if created != 0 {
			t.Fatalf("created = %v, want 0", created)
		}
		if rotates != 1 {
			t.Fatalf("rotation_supported = %v, want 1", rotates)
		}
	})
}
