package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestQueryDurationExportsNamedAndAdhocOutcomes(t *testing.T) {
	QueryDuration("libraryArtifactsBrowse", 250*time.Millisecond, false)
	QueryDuration("", time.Second, true)
	body := scrape(t)
	for _, want := range []string{
		`memql_query_duration_seconds_count{outcome="success",query="libraryArtifactsBrowse"}`,
		`memql_query_duration_seconds_count{outcome="error",query="<adhoc>"}`,
		`memql_query_duration_seconds_bucket{outcome="success",query="libraryArtifactsBrowse",le="0.25"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing latency series: %s", want)
		}
	}
}
