package sitetraffic

import (
	"math"
	"reflect"
	"testing"
)

func TestBucketBoundaries(t *testing.T) {
	cases := []struct {
		ns   int64
		want int
	}{
		{0, 0},
		{500_000, 0},
		{1_000_000, 4},
		{2_000_000, 8},
		{30_000_000_000, 63},
	}
	for _, c := range cases {
		if got := histogramBucket(c.ns); got != c.want {
			t.Errorf("histogramBucket(%d) = %d, want %d", c.ns, got, c.want)
		}
	}
}

func TestPercentileOfKnownDurations(t *testing.T) {
	h := make([]int64, histogramBuckets)
	h[histogramBucket(10_000_000)] += 900
	h[histogramBucket(500_000_000)] += 100
	p50, ok := percentileMs(h, 0.5)
	if !ok || math.Abs(p50-10)/10 > 0.10 {
		t.Errorf("p50 = %v (%v), want within 10%% of 10", p50, ok)
	}
	p95, ok := percentileMs(h, 0.95)
	if !ok || math.Abs(p95-500)/500 > 0.10 {
		t.Errorf("p95 = %v (%v), want within 10%% of 500", p95, ok)
	}
	if _, ok := percentileMs(make([]int64, histogramBuckets), 0.5); ok {
		t.Error("an empty histogram must report false")
	}
}

func TestMergeIsElementwise(t *testing.T) {
	a := make([]int64, histogramBuckets)
	b := make([]int64, histogramBuckets)
	a[0], a[5], b[5], b[63] = 1, 2, 3, 4
	got := mergeHistograms(a, nil, b)
	want := make([]int64, histogramBuckets)
	want[0], want[5], want[63] = 1, 5, 4
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge = %v, want %v", got, want)
	}
}
