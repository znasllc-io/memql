package work

// Reinforce: the construct reliability's first production arithmetic (epic
// memql#5408, task memql#5410).

import (
	"math"
	"testing"
)

// The first success of a template that has earned nothing is worth a fifth of
// the way to certainty -- one good run is evidence, not proof.
func TestASuccessFromZeroEarnsAFifth(t *testing.T) {
	if got := Reinforce(0, true); math.Abs(got-0.2) > 1e-12 {
		t.Fatalf("Reinforce(0, success) = %v, want 0.2", got)
	}
	if got := Reinforce(0.5, true); math.Abs(got-0.6) > 1e-12 {
		t.Fatalf("Reinforce(0.5, success) = %v, want 0.6: a success closes a fifth of the REMAINING gap", got)
	}
}

// A failure costs a fifth of what was earned, so one bad replay dents a
// long-proven template rather than erasing it -- the ladder's demotion rules,
// not this number, decide when a template stops being served.
func TestAFailureFromCertaintyKeepsFourFifths(t *testing.T) {
	if got := Reinforce(1, false); math.Abs(got-0.8) > 1e-12 {
		t.Fatalf("Reinforce(1, failure) = %v, want 0.8", got)
	}
	if got := Reinforce(0, false); got != 0 {
		t.Fatalf("Reinforce(0, failure) = %v, want 0: there is nothing below nothing", got)
	}
}

// Reliability is a fraction, and compile ranks catalogued templates by it. A
// value that crept past 1 would outrank every honest template forever.
func TestFiftySuccessesApproachButNeverExceedOne(t *testing.T) {
	r := 0.0
	for i := 0; i < 50; i++ {
		next := Reinforce(r, true)
		if next <= r && r < 1 {
			t.Fatalf("success %d did not raise reliability: %v -> %v", i+1, r, next)
		}
		r = next
	}
	if r > 1 {
		t.Fatalf("fifty successes reached %v, past 1", r)
	}
	if r < 0.99 {
		t.Fatalf("fifty successes reached only %v; the average must approach 1", r)
	}
}

// The stored value is read off a row, and a row can hold anything. A NaN would
// poison every later average (NaN plus anything is NaN) and a negative would
// sort below templates that never ran, so both read as the floor.
func TestANaNOrNegativeReliabilityClampsToZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      float64
		success bool
		want    float64
	}{
		{"NaN, then a failure", math.NaN(), false, 0},
		{"NaN, then a success", math.NaN(), true, 0.2},
		{"negative, then a failure", -3, false, 0},
		{"negative, then a success", -3, true, 0.2},
		{"above one, then a failure", 7, false, 0.8},
		{"infinity, then a success", math.Inf(1), true, 1},
	} {
		got := Reinforce(tc.in, tc.success)
		if math.IsNaN(got) || math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: Reinforce(%v, %v) = %v, want %v", tc.name, tc.in, tc.success, got, tc.want)
		}
	}
}
