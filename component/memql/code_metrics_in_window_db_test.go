package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// code_metrics_in_window_db_test.go -- the codeMetricsInWindow read
// (dsl/observability/queries.memql, memql#4208) against a real Postgres,
// which is the only place the `startsWith` predicate's SQL (`^@ ANY`) can
// be shown to select what the in-process evaluator says it selects.
//
// What it proves, beyond "the query runs":
//
//   - prefix selection is ANY-of over the list, scoped by bucket and by the
//     half-open window;
//   - the exact codeReference is an ADDITIONAL equality, not a mapping;
//   - an empty list with no codeReference returns nothing -- the
//     server-side guarantee the portal relies on -- and so does a list of
//     blanks;
//   - a LIKE metacharacter in a prefix is literal;
//   - an absent prefixes list is refused, not widened.
//
// Postgres-gated: skips when no DB is reachable, like the other
// sharedReadMergeEngine borrowers; MEMQL_REQUIRE_DB=1 turns the skip into a
// failure.

const codeMetricInWindowStart = "2030-01-01T00:00:00Z"
const codeMetricInWindowEnd = "2030-01-02T00:00:00Z"

// seedCodeMetric inserts one codeMetric row through the raw insert() path
// (no mutation is bound to the concept -- the continuous aggregates feed
// it) and returns the codeReference it carries.
func seedCodeMetric(t *testing.T, eng *MemQLEngine, ctx context.Context, id, codeReference, bucket, windowStart string) string {
	t.Helper()
	windowEnd := windowStart
	if ts, err := time.Parse(time.RFC3339, windowStart); err == nil {
		span := time.Hour
		if bucket == "1m" {
			span = time.Minute
		}
		windowEnd = ts.Add(span).UTC().Format(time.RFC3339)
	}
	expr := fmt.Sprintf(
		`insert("v1:observability:codeMetric", id=%q, payload={"codeReference":%q,"windowStart":%q,"windowEnd":%q,"bucket":%q,"callCount":3,"errorCount":1,"errorRate":0.33,"p50DurationNs":1000,"p95DurationNs":5000,"p99DurationNs":9000,"totalDurationNs":12000})`,
		id, codeReference, windowStart, windowEnd, bucket)
	if _, err := eng.Execute(ctx, expr); err != nil {
		t.Fatalf("seed codeMetric %s: %v", id, err)
	}
	return codeReference
}

// codeReferencesOf collects the codeReference payload field of every
// returned row, sorted for stable comparison.
func codeReferencesOf(res *ExecuteResult) []string {
	var out []string
	if res == nil || res.Bundle == nil {
		return out
	}
	for _, node := range res.Bundle.Nodes {
		if ref, ok := node.GetPayload().AsMap()["codeReference"].(string); ok {
			out = append(out, ref)
		}
	}
	return out
}

func TestCodeMetricsInWindow_SelectsByPrefixBucketAndWindow(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)

	// Unique namespace per run so rows from earlier runs (the raw insert
	// read-merges onto an existing id, but a prior run's other ids stay)
	// can never satisfy this run's prefixes.
	run := fmt.Sprintf("t4208%d", time.Now().UnixNano())
	base := "integration." + run + "."
	method := "method:" + run + ".(*Handler).Serve"

	email := seedCodeMetric(t, eng, ctx, run+"-email", base+"email.send", "1h", "2030-01-01T10:00:00Z")
	shopify := seedCodeMetric(t, eng, ctx, run+"-shopify", base+"shopify.sync", "1h", "2030-01-01T11:00:00Z")
	wild := seedCodeMetric(t, eng, ctx, run+"-wild", base+"%wild", "1h", "2030-01-01T12:00:00Z")
	exact := seedCodeMetric(t, eng, ctx, run+"-method", method, "1h", "2030-01-01T13:00:00Z")
	// Same prefix, wrong bucket / outside the window: never returned.
	seedCodeMetric(t, eng, ctx, run+"-email-1m", base+"email.send", "1m", "2030-01-01T10:05:00Z")
	seedCodeMetric(t, eng, ctx, run+"-email-late", base+"email.send", "1h", "2030-01-02T00:00:00Z")
	seedCodeMetric(t, eng, ctx, run+"-email-early", base+"email.send", "1h", "2029-12-31T23:00:00Z")

	call := func(prefixes []string, codeReference string) []string {
		t.Helper()
		quoted := make([]string, 0, len(prefixes))
		for _, p := range prefixes {
			quoted = append(quoted, fmt.Sprintf("%q", p))
		}
		args := fmt.Sprintf(`prefixes: [%s], bucket: "1h", windowStart: %q, windowEnd: %q`,
			strings.Join(quoted, ", "), codeMetricInWindowStart, codeMetricInWindowEnd)
		if codeReference != "" {
			args += fmt.Sprintf(`, codeReference: %q`, codeReference)
		}
		res, err := eng.Execute(ctx, "query codeMetricsInWindow("+args+")")
		require.NoError(t, err, "codeMetricsInWindow(%s)", args)
		return codeReferencesOf(res)
	}

	t.Run("one prefix", func(t *testing.T) {
		require.ElementsMatch(t, []string{email}, call([]string{base + "email."}, ""))
	})
	t.Run("a wider prefix selects every row under it, in this bucket and window only", func(t *testing.T) {
		require.ElementsMatch(t, []string{email, shopify, wild}, call([]string{base}, ""))
	})
	t.Run("a list is ANY-of", func(t *testing.T) {
		require.ElementsMatch(t, []string{email, shopify}, call([]string{base + "email.", base + "shopify."}, ""))
	})
	t.Run("the exact codeReference is an additional equality", func(t *testing.T) {
		require.ElementsMatch(t, []string{exact}, call([]string{}, method))
		require.ElementsMatch(t, []string{email, exact}, call([]string{base + "email."}, method))
	})
	t.Run("empty prefixes and no codeReference return nothing", func(t *testing.T) {
		require.Empty(t, call([]string{}, ""))
	})
	t.Run("a blank prefix is not a prefix", func(t *testing.T) {
		require.Empty(t, call([]string{""}, ""))
		require.Empty(t, call([]string{"", "  "}, ""))
		require.ElementsMatch(t, []string{email}, call([]string{"", base + "email."}, ""))
	})
	t.Run("a LIKE metacharacter in a prefix is literal", func(t *testing.T) {
		require.ElementsMatch(t, []string{wild}, call([]string{base + "%"}, ""))
		require.Empty(t, call([]string{base + "_mail."}, ""))
	})
	t.Run("an unknown exact key with empty prefixes returns nothing", func(t *testing.T) {
		require.Empty(t, call([]string{}, "method:"+run+".nothing"))
	})
}

func TestCodeMetricsInWindow_RefusesAnAbsentPrefixList(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	_, err := eng.Execute(ctx, fmt.Sprintf(
		`query codeMetricsInWindow(bucket: "1h", windowStart: %q, windowEnd: %q)`,
		codeMetricInWindowStart, codeMetricInWindowEnd))
	require.Error(t, err, "an absent prefixes list must be refused, never read as unconstrained")
	require.Contains(t, err.Error(), "prefixes")
}

func TestCodeMetricsInWindow_RefusesAnUnknownBucket(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	_, err := eng.Execute(ctx, fmt.Sprintf(
		`query codeMetricsInWindow(prefixes: ["integration."], bucket: "5m", windowStart: %q, windowEnd: %q)`,
		codeMetricInWindowStart, codeMetricInWindowEnd))
	require.Error(t, err)
	require.Contains(t, err.Error(), "bucket")
}
