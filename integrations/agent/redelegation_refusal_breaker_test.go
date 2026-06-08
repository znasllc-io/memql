package agent

import "testing"

// TestRedelegationRefusalBreaker_AbortsAtCeiling: a tool refused `max` times in
// a turn trips exactly at the ceiling (memql#1138). Default ceiling is 2.
func TestRedelegationRefusalBreaker_AbortsAtCeiling(t *testing.T) {
	b := &redelegationRefusalBreaker{max: 2, counts: map[string]int{}}

	if trip, count := b.observeRefusal("produceArtifact"); trip || count != 1 {
		t.Fatalf("1st refusal: trip=%v count=%d, want false/1", trip, count)
	}
	if trip, count := b.observeRefusal("produceArtifact"); !trip || count != 2 {
		t.Fatalf("2nd refusal: trip=%v count=%d, want true/2", trip, count)
	}
}

// TestRedelegationRefusalBreaker_ArgsIndependent: the breaker keys on tool name
// only, so the model varying args between refused calls still trips it -- this
// is the gap the args-keyed repeatFailureBreaker left open (memql#1138).
func TestRedelegationRefusalBreaker_PerToolIndependent(t *testing.T) {
	b := &redelegationRefusalBreaker{max: 2, counts: map[string]int{}}

	// A different tool name keeps its own count -- refusing toolA once then
	// toolB once must NOT trip either.
	if trip, _ := b.observeRefusal("produceArtifact"); trip {
		t.Fatal("produceArtifact 1st refusal should not trip")
	}
	if trip, _ := b.observeRefusal("someOtherTool"); trip {
		t.Fatal("someOtherTool 1st refusal should not trip")
	}
	// produceArtifact's 2nd refusal trips (count is per-tool, not global).
	if trip, count := b.observeRefusal("produceArtifact"); !trip || count != 2 {
		t.Fatalf("produceArtifact 2nd refusal: trip=%v count=%d, want true/2", trip, count)
	}
}

// TestRedelegationRefusalBreaker_Disabled: a non-positive ceiling disables the
// breaker (it never trips) but still counts for logging.
func TestRedelegationRefusalBreaker_Disabled(t *testing.T) {
	b := &redelegationRefusalBreaker{max: 0, counts: map[string]int{}}
	for i := 1; i <= 5; i++ {
		if trip, count := b.observeRefusal("produceArtifact"); trip || count != i {
			t.Fatalf("disabled refusal %d: trip=%v count=%d, want false/%d", i, trip, count, i)
		}
	}
}

// TestRedelegationRefusalBreaker_NilSafe: a nil breaker never panics / trips.
func TestRedelegationRefusalBreaker_NilSafe(t *testing.T) {
	var b *redelegationRefusalBreaker
	if trip, count := b.observeRefusal("produceArtifact"); trip || count != 0 {
		t.Fatalf("nil breaker: trip=%v count=%d, want false/0", trip, count)
	}
}
