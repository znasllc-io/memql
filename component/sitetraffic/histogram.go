package sitetraffic

import (
	"fmt"
	"math"
	"strings"
)

// The response-time histogram a statistics rollup stores per hour and per day.
//
// WHY A HISTOGRAM. A median of medians is not a median, so a percentile over
// an arbitrary range cannot be built from per-hour percentiles. Counting
// durations into fixed buckets can be summed, so any range's percentile is
// merged from its buckets.
//
// WHY 64 LOG-SCALE BUCKETS. Four buckets per doubling starting at 0.5 ms puts
// each bucket's edges a factor of 2^(1/4), about 19%, apart; interpolating
// geometrically inside the bucket keeps a percentile within about 10% of the
// truth. 64 buckets reach 0.5 ms * 2^16, about 33 s, and the last bucket
// absorbs anything slower.
const (
	histogramBuckets = 64
	histogramBaseNs  = 500_000
)

// histogramBucket is clamp(floor(4*log2(max(ns,1)/500000)), 0, 63). The SQL
// twin below must agree; TestSQLBucketMatchesGo pins them together.
func histogramBucket(durationNs int64) int {
	if durationNs < 1 {
		durationNs = 1
	}
	b := int(math.Floor(4 * math.Log2(float64(durationNs)/histogramBaseNs)))
	if b < 0 {
		return 0
	}
	if b > histogramBuckets-1 {
		return histogramBuckets - 1
	}
	return b
}

// histogramBucketSQL is the same bucketing as an expression over a bigint
// nanosecond column, for the rollup job's aggregate query. The column name is
// interpolated, so it must be a trusted identifier, never user input.
func histogramBucketSQL(column string) string {
	return fmt.Sprintf("LEAST(63, GREATEST(0, floor(4 * ln(GREATEST(%s,1)::float8 / 500000) / ln(2))))::int", column)
}

// histogramSumSQL is mergeHistograms as one SQL aggregate over a group of
// stored histograms: element i of the answer is the sum of element i of every
// row, a missing element counting as zero. One aggregate per element, in one
// pass over the rows, is what lets a statistics read merge a period's
// histograms in the database rather than pulling one 64-element array per
// site and day out of it. TestHistogramSumInSQLMatchesTheMergeInGo pins it to
// mergeHistograms. The column is interpolated: a trusted identifier only.
func histogramSumSQL(column string) string {
	var b strings.Builder
	b.WriteString("ARRAY[")
	for i := 1; i <= histogramBuckets; i++ { // SQL arrays are 1-based
		if i > 1 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "coalesce(sum(%s[%d]), 0)", column, i)
	}
	b.WriteString("]::bigint[]")
	return b.String()
}

// mergeHistograms adds histograms element by element. Missing or short inputs
// count as zeros, so a NULL-ish row never breaks a range read.
func mergeHistograms(hs ...[]int64) []int64 {
	out := make([]int64, histogramBuckets)
	for _, h := range hs {
		for i, v := range h {
			if i >= histogramBuckets {
				break
			}
			out[i] += v
		}
	}
	return out
}

// percentileMs returns the q-quantile (0..1) in milliseconds, or false when
// the histogram holds nothing. The answer is interpolated geometrically
// inside the bucket the quantile falls in, matching the log-scale spacing.
func percentileMs(h []int64, q float64) (float64, bool) {
	var total int64
	for _, v := range h {
		total += v
	}
	if total <= 0 {
		return 0, false
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	target := q * float64(total)
	var cum float64
	for b, v := range h {
		if v <= 0 {
			continue
		}
		if cum+float64(v) >= target {
			frac := (target - cum) / float64(v)
			lo := float64(histogramBaseNs) * math.Pow(2, float64(b)/4)
			hi := float64(histogramBaseNs) * math.Pow(2, float64(b+1)/4)
			ns := lo * math.Pow(hi/lo, frac)
			return ns / 1e6, true
		}
		cum += float64(v)
	}
	return 0, false
}
