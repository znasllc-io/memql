package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scrape renders the /metrics body the way an operator's curl would.
func scrape(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics: %v", err)
	}
	return string(body)
}

// THE METRIC NAMES ARE THE CONTRACT. An operator's runbook, a dashboard and
// an alert rule all name these strings, and a rename breaks all three
// silently -- the query just returns no data, which reads as "the system is
// idle" rather than "the series moved". Asserting the exact names makes a
// rename a deliberate act with a failing test attached.
//
// The epic's acceptance criterion is literally `curl :PORT/metrics | grep
// memql_result_cache`, which is why the result cache gets its own subsystem
// rather than a `cache="result"` label on a shared family.
func TestResultCacheMetricNamesAreStable(t *testing.T) {
	RegisterResultCacheStatsSource(func() CacheStats {
		return CacheStats{Hits: 7, Misses: 3, KeysAdded: 10, KeysEvicted: 2, CostAdded: 100, CostEvicted: 20}
	})
	t.Cleanup(func() { RegisterResultCacheStatsSource(nil) })

	ResultCacheInvalidationEviction(4)
	ResultCacheQueryRead("spaceParticipants", true)

	body := scrape(t)
	for _, want := range []string{
		"memql_result_cache_hits_total",
		"memql_result_cache_misses_total",
		"memql_result_cache_keys_added_total",
		"memql_result_cache_keys_evicted_total",
		"memql_result_cache_cost_added_total",
		"memql_result_cache_cost_evicted_total",
		"memql_result_cache_invalidation_evictions_total",
		"memql_result_cache_invalidation_events_total",
		"memql_result_cache_query_reads_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q -- a runbook, dashboard or alert naming it would silently return no data", want)
		}
	}

	// The exported values must be the SOURCE's values, not a second count.
	if !strings.Contains(body, "memql_result_cache_hits_total 7") {
		t.Error("hits_total did not report the registered snapshot's value 7")
	}
	if !strings.Contains(body, "memql_result_cache_misses_total 3") {
		t.Error("misses_total did not report the registered snapshot's value 3")
	}
}

// A cache that has registered no source contributes NO series. A hard zero
// would be a lie in the one direction that matters: it looks like a cache
// that exists and is never hit, which is exactly the incident an operator
// would then go hunting for.
func TestUnregisteredCacheExportsNoSeries(t *testing.T) {
	RegisterResultCacheStatsSource(nil)
	RegisterAICacheStatsSource(AICacheResponse, nil)
	RegisterAICacheStatsSource(AICacheSemantic, nil)

	body := scrape(t)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "memql_result_cache_hits_total ") ||
			strings.HasPrefix(line, "memql_ai_cache_hits_total{") {
			t.Errorf("an unregistered cache still exported a series: %q", line)
		}
	}
}

func TestAICacheSourcesAreLabelledIndependently(t *testing.T) {
	RegisterAICacheStatsSource(AICacheResponse, func() CacheStats { return CacheStats{Hits: 11, Misses: 1} })
	RegisterAICacheStatsSource(AICacheSemantic, func() CacheStats { return CacheStats{Hits: 22, Misses: 2} })
	t.Cleanup(func() {
		RegisterAICacheStatsSource(AICacheResponse, nil)
		RegisterAICacheStatsSource(AICacheSemantic, nil)
	})

	body := scrape(t)
	if !strings.Contains(body, `memql_ai_cache_hits_total{cache="response"} 11`) {
		t.Error("the exact-match AI cache did not export its own labelled series")
	}
	if !strings.Contains(body, `memql_ai_cache_hits_total{cache="semantic"} 22`) {
		t.Error("the semantic AI cache did not export its own labelled series")
	}
}

func TestResultCacheInvalidationCountsEventsAndEvictionsSeparately(t *testing.T) {
	events0, evictions0 := ResultCacheInvalidationValues()

	// An event that evicted nothing still proves invalidation REACHED this
	// replica -- the distinction the two series exist to draw.
	ResultCacheInvalidationEviction(0)
	events1, evictions1 := ResultCacheInvalidationValues()
	if events1-events0 != 1 {
		t.Fatalf("events counter delta = %v, want 1", events1-events0)
	}
	if evictions1-evictions0 != 0 {
		t.Fatalf("an event that evicted nothing incremented the evictions counter by %v", evictions1-evictions0)
	}

	ResultCacheInvalidationEviction(3)
	events2, evictions2 := ResultCacheInvalidationValues()
	if events2-events1 != 1 {
		t.Fatalf("events counter delta = %v, want 1", events2-events1)
	}
	if evictions2-evictions1 != 3 {
		t.Fatalf("evictions counter delta = %v, want 3", evictions2-evictions1)
	}
}

// The per-query series is what makes "name a query, get its hit ratio"
// answerable. Its two label values must be tracked independently or the
// ratio is meaningless.
func TestResultCacheQueryReadTracksHitAndMissSeparately(t *testing.T) {
	const q = "sitesAll"
	hit0 := ResultCacheQueryReadValue(q, true)
	miss0 := ResultCacheQueryReadValue(q, false)

	ResultCacheQueryRead(q, true)
	ResultCacheQueryRead(q, true)
	ResultCacheQueryRead(q, false)

	if got := ResultCacheQueryReadValue(q, true) - hit0; got != 2 {
		t.Errorf("cached=true delta = %v, want 2", got)
	}
	if got := ResultCacheQueryReadValue(q, false) - miss0; got != 1 {
		t.Errorf("cached=false delta = %v, want 1", got)
	}
}

// CARDINALITY. The per-query label is bounded only because it carries a
// registered construct NAME. An unnamed (ad-hoc) read must fold into one
// fixed bucket -- if it ever carried query text instead, every distinct
// argument value would mint a series and a user id could land in a label.
func TestAdhocReadsFoldIntoOneBucket(t *testing.T) {
	before := ResultCacheQueryReadValue(AdhocQueryLabel, false)

	ResultCacheQueryRead("", false)
	ResultCacheQueryRead("", false)

	if got := ResultCacheQueryReadValue(AdhocQueryLabel, false) - before; got != 2 {
		t.Fatalf("ad-hoc reads did not fold into %q: delta = %v, want 2", AdhocQueryLabel, got)
	}
	if strings.Contains(scrape(t), `query=""`) {
		t.Error("an empty query label reached the scrape; ad-hoc reads must carry the fixed bucket label")
	}
}
