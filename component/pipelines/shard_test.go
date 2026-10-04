package pipelines

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

// Longest-processing-time first: the heaviest measured package goes first,
// each into the shard that is lightest at that moment, ties to the lower
// index; unmeasured packages follow, by name, weighing UnknownSeconds each.
func TestPartitionPlacesLongestFirstIntoTheLightestShard(t *testing.T) {
	seconds := map[string]float64{
		"a": 496.00,
		"b": 445.63,
		"c": 345.97,
		"d": 283.19,
		"e": 119.09,
	}
	// a, b, c open the three shards; d joins c (345.97), the lightest; e joins
	// b (445.63); f and g, unmeasured, both join a (496, then 501).
	got := Partition([]string{"g", "f", "e", "d", "c", "b", "a"}, seconds, 3)
	want := [][]string{{"a", "f", "g"}, {"b", "e"}, {"c", "d"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Partition = %v, want %v", got, want)
	}
}

// The CI bridge places every measured package before any unmeasured one; it
// does NOT sort an unmeasured package in among the measured at its nominal
// weight. The two orders disagree here, and the parity test against
// scripts/ci/selection.Partition needs the bridge's.
func TestPartitionPlacesUnmeasuredPackagesAfterMeasuredOnes(t *testing.T) {
	seconds := map[string]float64{"a": 10, "h": 4}
	// Bridge order: a -> 0 (10), h -> 1 (4), u1 -> 1 (9), u2 -> 1 (14).
	// Sorting u1 and u2 in at 5s would give [[a h] [u1 u2]] instead.
	got := Partition([]string{"a", "h", "u1", "u2"}, seconds, 2)
	want := [][]string{{"a"}, {"h", "u1", "u2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Partition = %v, want %v", got, want)
	}
}

func TestPartitionBreaksTiesByNameAndByLowerIndex(t *testing.T) {
	seconds := map[string]float64{"a": 10, "b": 10, "c": 10}
	for _, order := range [][]string{{"a", "b", "c"}, {"c", "b", "a"}, {"b", "c", "a"}} {
		got := Partition(order, seconds, 2)
		// a and b open the two shards; c, tied at 10 against 10, joins the
		// lower index.
		want := [][]string{{"a", "c"}, {"b"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Partition(%v) = %v, want %v", order, got, want)
		}
	}
}

func TestPartitionCapsTheShardCount(t *testing.T) {
	three := []string{"x", "y", "z"}
	if got, want := Partition(three, nil, 8), [][]string{{"x"}, {"y"}, {"z"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("n above the package count: Partition = %v, want one package per shard %v", got, want)
	}
	for _, n := range []int{0, -3, 1} {
		if got, want := Partition(three, nil, n), [][]string{{"x", "y", "z"}}; !reflect.DeepEqual(got, want) {
			t.Errorf("n = %d: Partition = %v, want one shard %v", n, got, want)
		}
	}
	var many []string
	for i := range 20 {
		many = append(many, fmt.Sprintf("p%02d", i))
	}
	if got := Partition(many, nil, 50); len(got) != MaxShards {
		t.Errorf("n = 50 over 20 packages: %d shards, want MaxShards = %d", len(got), MaxShards)
	}
}

func TestPartitionOfNothingIsNoShards(t *testing.T) {
	if got := Partition(nil, map[string]float64{"a": 1}, 4); len(got) != 0 {
		t.Errorf("Partition(nil) = %v, want no shards", got)
	}
}

// Packages measured at zero all land in the first shard (it never stops being
// the lightest), which leaves the others empty. An empty shard would be a step
// with nothing to run, so it is dropped rather than returned.
func TestPartitionDropsEmptyShards(t *testing.T) {
	got := Partition([]string{"a", "b", "c"}, map[string]float64{"a": 0, "b": 0, "c": 0}, 3)
	if want := [][]string{{"a", "b", "c"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Partition = %v, want %v", got, want)
	}
}

// A time that is negative or not a finite number cannot be a wall time; it is
// read as unmeasured rather than allowed to steer the greedy (a negative
// weight would make its shard the lightest forever).
func TestPartitionReadsAnImpossibleTimeAsUnmeasured(t *testing.T) {
	// As unmeasured, b lands like c and d: a -> 0, b -> 1, c -> 1, d -> 0.
	want := [][]string{{"a", "d"}, {"b", "c"}}
	for _, bad := range []float64{math.NaN(), -50, math.Inf(1), math.Inf(-1)} {
		got := Partition([]string{"a", "b", "c", "d"}, map[string]float64{"a": 10, "b": bad}, 2)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("b timed at %v: Partition = %v, want %v", bad, got, want)
		}
	}
}

// The answer depends on the set of packages and their times, never on the
// order they arrive in, on duplicates, or on how a map happens to iterate.
func TestPartitionIsDeterministicAndLossless(t *testing.T) {
	pairs := []struct {
		pkg string
		sec float64
	}{
		{"example.test/m/a", 30}, {"example.test/m/b", 30}, {"example.test/m/c", 12.5},
		{"example.test/m/d", 7}, {"example.test/m/e", 7}, {"example.test/m/f", 1},
	}
	input := []string{
		"example.test/m/u2", "example.test/m/f", "example.test/m/a", "example.test/m/u1",
		"example.test/m/e", "example.test/m/c", "example.test/m/b", "example.test/m/d",
	}
	var first [][]string
	for i := range 50 {
		seconds := map[string]float64{} // a fresh map iterates in a fresh order
		for j := range pairs {
			p := pairs[(i+j)%len(pairs)]
			seconds[p.pkg] = p.sec
		}
		order := append([]string(nil), input[i%len(input):]...)
		order = append(order, input[:i%len(input)]...)
		order = append(order, input[i%len(input)]) // and one duplicate
		got := Partition(order, seconds, 3)
		if first == nil {
			first = got
			continue
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("Partition is not deterministic:\n%v\n%v", first, got)
		}
	}
	placed := map[string]int{}
	for _, shard := range first {
		for _, pkg := range shard {
			placed[pkg]++
		}
	}
	for _, pkg := range input {
		if placed[pkg] != 1 {
			t.Errorf("%s is in %d shards, want exactly 1", pkg, placed[pkg])
		}
	}
	if len(placed) != len(input) {
		t.Errorf("%d packages placed, want %d", len(placed), len(input))
	}
}
