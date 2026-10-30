package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// The series is what section 10 of the access codes design points an operator at, and
// a counter that is declared and not registered exports nothing and reads as nobody
// having called. Its value helper is what the handler's test leans on, so this is what
// says the two agree with the scrape.
func TestSiteAccessRequestsAreExported(t *testing.T) {
	before := SiteAccessRequestValue("unlock", "invalid")
	SiteAccessRequest("unlock", "invalid")
	if got := SiteAccessRequestValue("unlock", "invalid"); got != before+1 {
		t.Fatalf("value = %v, want %v", got, before+1)
	}

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	want := `memql_site_access_requests_total{op="unlock",result="invalid"}`
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("/metrics does not carry %s", want)
	}
}

// THE HELP IS WHAT AN ALERT IS WRITTEN FROM (signing_key_help_test.go). A visitor can move
// call/refused at will, with a wrong argument type or a code with no subject on a gate
// whose constructs use one, so a Help that called every refused the graph failing would
// have an operator paged by a visitor.
func TestSiteAccessHelpDoesNotCallEveryRefusalAFault(t *testing.T) {
	SiteAccessRequest("call", "refused")
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	help := ""
	for _, family := range families {
		if family.GetName() == "memql_site_access_requests_total" {
			help = family.GetHelp()
		}
	}
	if help == "" {
		t.Fatal("memql_site_access_requests_total is not exported, or has no Help")
	}
	for _, want := range []string{"call/refused", "DO NOT ALERT", "wrong argument type", "no subject", "unlock, session or leave"} {
		if !strings.Contains(help, want) {
			t.Errorf("the Help does not say %q: %s", want, help)
		}
	}
	if strings.Contains(help, "any refused") {
		t.Errorf("the Help still reads every refused as a fault: %s", help)
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

// TestFederationExchangeSeriesExistFromBoot is the reachable positive for the
// zero-init in init(): the runbook's "alert on denied" rule needs the series
// to exist before the first denial, or it evaluates to no data on exactly the
// cluster it is watching.
func TestFederationExchangeSeriesExistFromBoot(t *testing.T) {
	// EVERY (vendor, outcome) PAIR, not every outcome. The counter gained a
	// vendor label in memql#5088, and checking outcomes alone would pass while
	// only ONE vendor's series existed -- which is the exact failure this test
	// is about, one vendor further on: an operator alerting on
	// {vendor="openai",outcome="denied"} would get no data, and "openai has
	// never been denied" and "openai is not scraped" would look identical.
	families, err := Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	seen := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "memql_ai_federation_exchanges_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var vendor, outcome string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "vendor":
					vendor = l.GetValue()
				case "outcome":
					outcome = l.GetValue()
				}
			}
			seen[vendor+"/"+outcome] = true
		}
	}

	for _, vendor := range []string{FederationVendorAnthropic, FederationVendorOpenAI} {
		for _, outcome := range []string{
			FederationExchangeOK, FederationExchangeDenied, FederationExchangeError,
		} {
			if !seen[vendor+"/"+outcome] {
				t.Errorf("memql_ai_federation_exchanges_total{vendor=%q,outcome=%q} does not exist "+
					"before the first exchange; an alert over it would evaluate to no data",
					vendor, outcome)
			}
		}
	}
}

// TestStatusHistoryVersionsPurgedCountsPerConcept pins what the purge's counter
// promises an operator: each concept is counted on its own, and a batch that
// removed nothing is not a sample.
func TestStatusHistoryVersionsPurgedCountsPerConcept(t *testing.T) {
	registration, clusterNode := "v1:worker:registration", "v1:cluster:node"
	beforeRegistration := StatusHistoryVersionsPurgedValue(registration)
	beforeNode := StatusHistoryVersionsPurgedValue(clusterNode)

	StatusHistoryVersionsPurged(registration, 500)
	StatusHistoryVersionsPurged(registration, 12)
	StatusHistoryVersionsPurged(clusterNode, 3)
	// Nothing removed is not a sample, and a counter can never go down: Add
	// panics on a negative, so the recorder must never pass one through.
	StatusHistoryVersionsPurged(clusterNode, 0)
	StatusHistoryVersionsPurged(clusterNode, -4)

	if got := StatusHistoryVersionsPurgedValue(registration) - beforeRegistration; got != 512 {
		t.Fatalf("registration counted %v versions, want 512", got)
	}
	if got := StatusHistoryVersionsPurgedValue(clusterNode) - beforeNode; got != 3 {
		t.Fatalf("cluster node counted %v versions, want 3 (zero and negative are not samples)", got)
	}
}

// TestStatusHistorySeriesExistFromBoot: the Help string tells an operator to
// alert on this counter going quiet, and a rule over a series that does not
// exist yet evaluates to no data rather than to zero -- the state of a purge
// that has never run. So every concept is exported at 0 before it is first used.
func TestStatusHistorySeriesExistFromBoot(t *testing.T) {
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var seen []string
	for _, mf := range mfs {
		if mf.GetName() != "memql_status_history_versions_purged_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "concept" {
					seen = append(seen, l.GetValue())
				}
			}
		}
	}
	for _, want := range StatusHistoryConcepts() {
		found := false
		for _, got := range seen {
			found = found || got == want
		}
		if !found {
			t.Errorf("no series for concept %q at boot; exported %v", want, seen)
		}
	}
	if len(seen) != len(StatusHistoryConcepts()) {
		t.Errorf("exported concepts %v, want exactly %v", seen, StatusHistoryConcepts())
	}
}
