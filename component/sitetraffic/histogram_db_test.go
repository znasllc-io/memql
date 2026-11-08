package sitetraffic

import (
	"context"
	"fmt"
	"testing"
)

// TestSQLBucketMatchesGo pins the rollup job's SQL bucketing to the Go
// function the readers use: a disagreement would shift every percentile.
func TestSQLBucketMatchesGo(t *testing.T) {
	db := edgeTestDB(t)
	if db == nil {
		return
	}
	durations := []int64{0, 1, 1000, 250_000, 499_999, 500_000, 700_000, 1_000_000, 1_500_000, 2_000_000,
		3_333_333, 10_000_000, 50_000_000, 123_456_789, 500_000_000, 1_000_000_000, 4_000_000_000,
		30_000_000_000, 600_000_000_000, 9_000_000_000_000}
	for _, d := range durations {
		var got int
		q := fmt.Sprintf("SELECT %s FROM (SELECT ?::bigint AS d) x", histogramBucketSQL("d"))
		if err := db.NewRaw(q, d).Scan(context.Background(), &got); err != nil {
			t.Fatalf("duration %d: %v", d, err)
		}
		if want := histogramBucket(d); got != want {
			t.Errorf("duration %d: SQL bucket %d, Go bucket %d", d, got, want)
		}
	}
}
