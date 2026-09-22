package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDSLDeprecatedUseReachesTheScrape: DSLDeprecatedUsesValue reads the
// counter itself, so it would count happily on a series nothing registered and
// no scrape sees. This reads /metrics, which is what an operator's alert reads.
func TestDSLDeprecatedUseReachesTheScrape(t *testing.T) {
	const rule = "deprecated_array_type"
	before := DSLDeprecatedUsesValue(rule)
	DSLDeprecatedUse(rule, 3)
	if got := DSLDeprecatedUsesValue(rule) - before; got != 3 {
		t.Fatalf("the counter rose by %v, want 3", got)
	}

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if want := `memql_dsl_deprecated_uses_total{rule="deprecated_array_type"}`; !strings.Contains(string(body), want) {
		t.Fatalf("/metrics does not expose %s", want)
	}
}

// A load that finds no use of a rule creates its series at zero rather than
// counting, which is how every registered rule is scraped from boot. Without
// it an alert on a form nobody uses evaluates to no data, which reads exactly
// like the form being used and the counter never scraped.
func TestDSLDeprecatedUseOfZeroCreatesTheSeriesAtZero(t *testing.T) {
	const rule = "deprecated_probe_zero"
	DSLDeprecatedUse(rule, 0)
	DSLDeprecatedUse(rule, -2) // never negative: a counter only rises
	families, err := Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "memql_dsl_deprecated_uses_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "rule" && l.GetValue() == rule {
					if v := m.GetCounter().GetValue(); v != 0 {
						t.Fatalf("the series is %v, want 0", v)
					}
					return
				}
			}
		}
	}
	t.Fatalf("DSLDeprecatedUse(%q, 0) created no series", rule)
}
