package work

// reliability.go -- the arithmetic behind v1:authoring:construct.reliability
// (epic memql#5408, task memql#5410).
//
// The field has existed since the trust ladder was first sketched and has had
// no writer; the replay runner is its first, through recordConstructReliability.
// An exponential moving average: each replay moves the value a fifth of the way
// toward its outcome, so recent evidence counts most and no single replay
// decides anything.
//
// RELIABILITY RANKS; THE LADDER DECIDES. Compile sorts a goal's candidates by
// this number, and nothing is promoted, demoted or served because of it -- the
// ladder's counters and the policy row do that (ladder.go). Keeping the two
// apart is what lets a failure dent a long-proven template without erasing it
// while two failures in a row still demote it.
//
// This is the TEMPLATE's reliability. component/actions/fingerprint keeps an
// action's, with its own gain; the two are different subjects and are not
// merged (v1:authoring:construct.reliability says so).

import "math"

// reinforceAlpha is the moving average's weight on the newest replay.
const reinforceAlpha = 0.2

// Reinforce returns the reliability after one replay: a success closes a fifth
// of the gap to 1, a failure keeps four fifths of what was earned. The stored
// value is read off a row, so it is clamped into [0, 1] first -- a NaN would
// poison every later average and a negative would rank below templates that
// never ran.
func Reinforce(reliability float64, success bool) float64 {
	r := clampUnit(reliability)
	if success {
		r += reinforceAlpha * (1 - r)
	} else {
		r *= 1 - reinforceAlpha
	}
	return clampUnit(r)
}

// clampUnit bounds a value to [0, 1], reading NaN as 0.
func clampUnit(x float64) float64 {
	switch {
	case math.IsNaN(x) || x < 0:
		return 0
	case x > 1:
		return 1
	}
	return x
}
