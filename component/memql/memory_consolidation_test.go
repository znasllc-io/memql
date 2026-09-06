package memql

import (
	"reflect"
	"testing"
	"time"
)

// Tests for the pure memory-consolidation decision helpers (#586).
// They cover the acceptance criteria that hinge on a DECISION rather
// than on the DB / LLM path: reinforce-vs-create dedup, decay over
// time, prune cutoff, provenance growth, and the incremental
// watermark. No database, no LLM -- pure logic, mirroring
// TestStepTransitionAllowed.

func TestConsolidationShouldReinforce(t *testing.T) {
	const thr = ConsolidationDefaultDedupThreshold
	cases := []struct {
		name        string
		cosine      float64
		matchActive bool
		want        bool
	}{
		{"exact restatement reinforces", 0.99, true, true},
		{"at threshold reinforces (re-run over same episodes)", thr, true, true},
		{"just below threshold creates new belief", thr - 0.01, true, false},
		{"unrelated belief creates new belief", 0.20, true, false},
		{"strong match but pruned never reinforces", 0.99, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ConsolidationShouldReinforce(c.cosine, thr, c.matchActive)
			if got != c.want {
				t.Fatalf("ConsolidationShouldReinforce(%v, %v, active=%v) = %v, want %v",
					c.cosine, thr, c.matchActive, got, c.want)
			}
		})
	}
}

func TestConsolidationReinforcedConfidence(t *testing.T) {
	cases := []struct {
		name  string
		prior float64
		bump  float64
		want  float64
	}{
		{"reinforcement raises confidence", 0.50, 0.15, 0.65},
		{"clamps at 1.0", 0.95, 0.15, 1.0},
		{"already at ceiling stays", 1.0, 0.15, 1.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ConsolidationReinforcedConfidence(c.prior, c.bump)
			if !approxEq(got, c.want) {
				t.Fatalf("ConsolidationReinforcedConfidence(%v, %v) = %v, want %v",
					c.prior, c.bump, got, c.want)
			}
		})
	}
}

func TestConsolidationDecayedConfidence(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	const days = ConsolidationDefaultDecayDays
	const per = ConsolidationDefaultDecayPerInterval

	cases := []struct {
		name  string
		prior float64
		last  time.Time
		want  float64
	}{
		{"fresh belief untouched", 0.80, now.Add(-1 * day), 0.80},
		{"just inside first window untouched", 0.80, now.Add(-(days - 1) * day), 0.80},
		{"one full window decays once", 0.80, now.Add(-days * day), 0.70},
		{"two full windows decay twice", 0.80, now.Add(-2 * days * day), 0.60},
		{"long-stale clamps at 0", 0.20, now.Add(-100 * days * day), 0.0},
		{"future lastReinforced is a no-op", 0.80, now.Add(day), 0.80},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ConsolidationDecayedConfidence(c.prior, c.last, now, days, per)
			if !approxEq(got, c.want) {
				t.Fatalf("ConsolidationDecayedConfidence(prior=%v, age=%v) = %v, want %v",
					c.prior, now.Sub(c.last), got, c.want)
			}
		})
	}
}

func TestConsolidationDecayDisabled(t *testing.T) {
	now := time.Now()
	// decayDays <= 0 disables decay entirely.
	if got := ConsolidationDecayedConfidence(0.8, now.Add(-1000*time.Hour), now, 0, 0.1); !approxEq(got, 0.8) {
		t.Fatalf("decayDays=0 should disable decay, got %v", got)
	}
}

func TestConsolidationShouldPrune(t *testing.T) {
	const floor = ConsolidationDefaultPruneConfidence
	cases := []struct {
		name       string
		confidence float64
		active     bool
		want       bool
	}{
		{"healthy belief survives", 0.50, true, false},
		{"at floor is pruned", floor, true, true},
		{"below floor is pruned", floor - 0.05, true, true},
		{"already pruned is left alone (idempotent)", 0.0, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ConsolidationShouldPrune(c.confidence, floor, c.active)
			if got != c.want {
				t.Fatalf("ConsolidationShouldPrune(%v, %v, active=%v) = %v, want %v",
					c.confidence, floor, c.active, got, c.want)
			}
		})
	}
}

// TestConsolidationDecayThenPrune walks the full forgetting path: a
// belief goes unreinforced long enough to decay below the floor, then
// the sweep prunes it. Acceptance criterion: "stale, unreinforced
// memories decay and are pruned after a configurable threshold."
func TestConsolidationDecayThenPrune(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	const days = ConsolidationDefaultDecayDays
	const per = ConsolidationDefaultDecayPerInterval
	const floor = ConsolidationDefaultPruneConfidence

	prior := 0.5
	last := now.Add(-4 * days * day) // 4 windows => 0.5 - 0.4 = 0.1
	decayed := ConsolidationDecayedConfidence(prior, last, now, days, per)
	if !approxEq(decayed, 0.1) {
		t.Fatalf("expected decayed 0.1, got %v", decayed)
	}
	if !ConsolidationShouldPrune(decayed, floor, true) {
		t.Fatalf("decayed belief at %v should be pruned (floor %v)", decayed, floor)
	}
}

func TestConsolidationMergeProvenance(t *testing.T) {
	cases := []struct {
		name     string
		existing []string
		incoming []string
		want     []string
	}{
		{
			name:     "appends new evidence, drops dupes, preserves order",
			existing: []string{"obs:a", "obs:b"},
			incoming: []string{"obs:b", "obs:c"},
			want:     []string{"obs:a", "obs:b", "obs:c"},
		},
		{
			name:     "re-run over identical episodes adds nothing",
			existing: []string{"obs:a", "obs:b"},
			incoming: []string{"obs:a", "obs:b"},
			want:     []string{"obs:a", "obs:b"},
		},
		{
			name:     "skips empty ids",
			existing: []string{"obs:a", ""},
			incoming: []string{"", "obs:c"},
			want:     []string{"obs:a", "obs:c"},
		},
		{
			name:     "first run seeds from incoming only",
			existing: nil,
			incoming: []string{"obs:x", "obs:x", "obs:y"},
			want:     []string{"obs:x", "obs:y"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ConsolidationMergeProvenance(c.existing, c.incoming)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ConsolidationMergeProvenance(%v, %v) = %v, want %v",
					c.existing, c.incoming, got, c.want)
			}
		})
	}
}

// TestConsolidationNextWatermark backs the incremental acceptance
// criterion: the watermark advances to the newest episode processed,
// and an empty batch leaves it unmoved (a no-op run does not lose
// ground).
func TestConsolidationNextWatermark(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	prior := base
	batch := []time.Time{
		base.Add(2 * time.Hour),
		base.Add(5 * time.Hour),
		base.Add(1 * time.Hour),
	}
	if got := ConsolidationNextWatermark(prior, batch); !got.Equal(base.Add(5 * time.Hour)) {
		t.Fatalf("watermark should advance to newest episode, got %v", got)
	}
	if got := ConsolidationNextWatermark(prior, nil); !got.Equal(prior) {
		t.Fatalf("empty batch should leave watermark at prior, got %v", got)
	}
	// A batch entirely at/below prior (already-consolidated stragglers)
	// must not move the watermark backward.
	old := []time.Time{base.Add(-3 * time.Hour), base}
	if got := ConsolidationNextWatermark(prior, old); !got.Equal(prior) {
		t.Fatalf("stale batch must not move watermark backward, got %v", got)
	}
}

func approxEq(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
