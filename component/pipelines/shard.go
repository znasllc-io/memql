package pipelines

import (
	"math"
	"slices"
	"strings"
)

// MaxShards caps how many steps one step may split into. Every shard is a Job
// with its own clone, image pull, cache restore and (for a database step) a
// Postgres sidecar; past a handful the setup outweighs the split. It is the
// CI bridge's maxShardsPerClass (scripts/ci/selection/shard.go).
const MaxShards = 8

// UnknownSeconds is the weight a package the timing table has not measured is
// placed with. It lands in the then lightest shard, as the design record asks
// (section 5, "a shard table drifts"); the nominal weight keeps several new
// packages from all landing in that same one.
const UnknownSeconds = 5.0

// Partition splits packages over at most n shards by longest-processing-time
// first: heaviest first, each into the currently lightest shard (ties: lower
// index). An unmeasured package weighs UnknownSeconds. Each shard is sorted;
// empty shards are dropped.
//
// It is the within-class greedy of the CI bridge's Partition
// (scripts/ci/selection/shard.go), ported step for step so a pipeline shards
// as the bridge does and the root module's parity test can hold the two
// together: measured packages are placed first, by seconds descending then
// name; every unmeasured package follows, by name, into the then lightest
// shard. n is capped at MaxShards and at the number of packages, and below 1
// it is 1.
//
// Three things the bridge refuses with an error are answered here instead,
// because a compile has no one to return an error to: a package listed twice
// is placed once, a shard left empty (packages measured at zero all land in
// the first) is dropped, and a time that is negative or not finite is read as
// unmeasured.
func Partition(packages []string, seconds map[string]float64, n int) [][]string {
	unique := slices.Clone(packages)
	slices.Sort(unique)
	unique = slices.Compact(unique)
	if len(unique) == 0 {
		return nil
	}
	n = max(1, min(n, MaxShards, len(unique)))

	var known, unknown []string
	for _, pkg := range unique {
		if s, ok := seconds[pkg]; ok && s >= 0 && !math.IsInf(s, 1) {
			known = append(known, pkg)
		} else {
			unknown = append(unknown, pkg) // already in name order
		}
	}
	slices.SortFunc(known, func(a, b string) int {
		if sa, sb := seconds[a], seconds[b]; sa != sb {
			if sa > sb {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})

	shards := make([][]string, n)
	load := make([]float64, n)
	lightest := func() int {
		best := 0
		for i := 1; i < n; i++ {
			if load[i] < load[best] {
				best = i
			}
		}
		return best
	}
	for _, pkg := range known {
		i := lightest()
		shards[i] = append(shards[i], pkg)
		load[i] += seconds[pkg]
	}
	for _, pkg := range unknown {
		i := lightest()
		shards[i] = append(shards[i], pkg)
		load[i] += UnknownSeconds
	}

	out := make([][]string, 0, n)
	for _, shard := range shards {
		if len(shard) == 0 {
			continue
		}
		slices.Sort(shard)
		out = append(out, shard)
	}
	return out
}
