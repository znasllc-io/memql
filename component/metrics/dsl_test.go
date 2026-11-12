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

// TestBelongsToUndeclaredReachesTheScrape: BelongsToUndeclaredValue reads the
// gauge itself, so it would answer happily for a gauge nothing registered and
// no scrape sees. This reads /metrics, which is what an operator's alert reads
// -- and it reads it BEFORE the first Set as well, because the healthy value is
// zero and a node with no product bundle has to export that zero rather than no
// series at all: a rule over a missing series evaluates to no data, which reads
// the same as a bundle nobody measured.
func TestBelongsToUndeclaredReachesTheScrape(t *testing.T) {
	scrape := func() string {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body, _ := io.ReadAll(rec.Body)
		return string(body)
	}
	if body := scrape(); !strings.Contains(body, "\nmemql_dsl_belongs_to_undeclared ") {
		t.Fatalf("/metrics does not expose memql_dsl_belongs_to_undeclared before any load set it")
	}

	BelongsToUndeclared(4)
	if got := BelongsToUndeclaredValue(); got != 4 {
		t.Fatalf("the gauge reads %v, want 4", got)
	}
	if body := scrape(); !strings.Contains(body, "\nmemql_dsl_belongs_to_undeclared 4\n") {
		t.Fatalf("/metrics does not expose the gauge at 4")
	}

	// A state, not a tally: a later load that finds fewer takes it down, and
	// one that finds none takes it to zero.
	BelongsToUndeclared(1)
	if got := BelongsToUndeclaredValue(); got != 1 {
		t.Fatalf("the gauge reads %v after a load that found 1, want 1", got)
	}
	BelongsToUndeclared(-3) // a count cannot be negative: ignored
	if got := BelongsToUndeclaredValue(); got != 1 {
		t.Fatalf("a negative count moved the gauge to %v", got)
	}
	BelongsToUndeclared(0)
	if got := BelongsToUndeclaredValue(); got != 0 {
		t.Fatalf("the gauge reads %v after a clean load, want 0", got)
	}
	if body := scrape(); !strings.Contains(body, "\nmemql_dsl_belongs_to_undeclared 0\n") {
		t.Fatalf("/metrics does not expose the gauge at 0 after a clean load")
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
