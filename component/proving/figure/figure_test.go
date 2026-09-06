package figure

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// ciProv is a complete CI provenance; liveProv a complete live one. Both are
// helpers rather than package vars so a test that mutates one cannot leak
// into another (share a fixture per file, never per package).
func ciProv() Provenance {
	return Provenance{Commit: "9e91625", Date: "2026-09-06", Tier: TierCI, Runner: "ubuntu-latest"}
}

func liveProv() Provenance {
	cost := 0.0412
	return Provenance{
		Commit: "9e91625", Date: "2026-09-06", Tier: TierLive,
		ModelIds: []string{"claude-sonnet-5", "gpt-5.4-mini"}, CostUSD: &cost, Runner: "ubuntu-latest",
	}
}

// --- Property 1: unmeasured is a value, not a zero -------------------------

func TestAFigureWithNeitherAStatNorAReasonIsRefused(t *testing.T) {
	// The whole package exists so that this shape cannot be published. A
	// caller who forgets to say why there is no number gets an error naming
	// the metric, not a printed 0.
	f := Figure{Metric: MetricProviderCalls, Unit: UnitCalls, Prov: ciProv()}
	if err := f.Validate(); !errors.Is(err, ErrNotAFigure) {
		t.Fatalf("Validate() = %v, want ErrNotAFigure", err)
	}
	rendered := f.Render()
	if !strings.Contains(rendered, "either a statistic or an absent reason") {
		t.Errorf("Render() = %q, want the error in the place the number would have been", rendered)
	}
	if strings.TrimSpace(rendered) == "0" {
		t.Error("Render() produced a bare zero for a figure that measured nothing")
	}
}

func TestAFigureCarryingBothAStatAndAReasonIsRefused(t *testing.T) {
	st, err := NewStat([]float64{1, 2, 3})
	if err != nil {
		t.Fatalf("NewStat: %v", err)
	}
	f := Figure{Metric: MetricProviderCalls, Unit: UnitCalls, Stat: &st, Absent: ReasonTierNotRun, Prov: ciProv()}
	if err := f.Validate(); !errors.Is(err, ErrBothStatAndReason) {
		t.Fatalf("Validate() = %v, want ErrBothStatAndReason", err)
	}
}

func TestAMeasuredZeroAndAnAbsentFigureRenderDifferently(t *testing.T) {
	// This is the assertion the whole design rests on: a run that genuinely
	// made zero provider calls and a run whose provider-call count could not
	// be measured must not read the same.
	measuredZero, err := Measured(MetricProviderCalls, []float64{0, 0, 0}, ciProv())
	if err != nil {
		t.Fatalf("Measured: %v", err)
	}
	// The detail is a STAND-IN, not a claim about this tree. It used to read
	// "DecideServe has no call site", which was true until memql#4999 built
	// that seam -- at which point a fixture was asserting something false
	// about the repository and nothing could tell.
	absent, err := Unmeasured(MetricProviderCalls, ReasonSeamNotBuilt, "<the seam this figure would count>", ciProv())
	if err != nil {
		t.Fatalf("Unmeasured: %v", err)
	}
	if measuredZero.Render() == absent.Render() {
		t.Fatalf("a measured zero and an unmeasured figure render identically as %q", absent.Render())
	}
	if !strings.HasPrefix(measuredZero.Render(), "0 ") {
		t.Errorf("a measured zero should lead with the number; got %q", measuredZero.Render())
	}
	if !strings.HasPrefix(absent.Render(), "--") {
		t.Errorf("an absent figure should lead with an em dash; got %q", absent.Render())
	}
	// The DETAIL is what an absent figure says when it has one -- the reason
	// alone is a category, and a category can be right about who can answer
	// while explaining the wrong mechanism (memql#4999).
	if !strings.Contains(absent.Render(), "<the seam this figure would count>") {
		t.Errorf("an absent figure must carry its detail; got %q", absent.Render())
	}
}

// With no detail there is still a reason, and it must reach the page: an
// absent figure that renders a bare em dash says nothing at all.
func TestAnAbsentFigureWithNoDetailStillCarriesItsReason(t *testing.T) {
	f, err := Unmeasured(MetricProviderCalls, ReasonTierNotRun, "", ciProv())
	if err != nil {
		t.Fatalf("Unmeasured: %v", err)
	}
	if !strings.Contains(f.Render(), ReasonTierNotRun.Sentence()) {
		t.Errorf("an absent figure with no detail dropped its reason; got %q", f.Render())
	}
}

func TestSeamNotBuiltMustNameTheMissingCode(t *testing.T) {
	// "The code is not built" without naming the code is the sort of absence
	// that never becomes a work item.
	if _, err := Unmeasured(MetricProviderCalls, ReasonSeamNotBuilt, "  ", ciProv()); err == nil {
		t.Fatal("Unmeasured accepted seamNotBuilt with no detail")
	}
	if _, err := Unmeasured(MetricProviderCalls, ReasonTierNotRun, "", ciProv()); err != nil {
		t.Fatalf("a reason that needs no detail was refused: %v", err)
	}
}

func TestTheAbsentReasonSetIsClosed(t *testing.T) {
	if _, err := Unmeasured(MetricProviderCalls, AbsentReason("n/a"), "", ciProv()); err == nil {
		t.Fatal("Unmeasured accepted a reason outside the closed set")
	}
	for _, r := range AllAbsentReasons() {
		if !r.Valid() {
			t.Errorf("%q is in AllAbsentReasons but Valid() says no", r)
		}
		if r.Sentence() == string(r) {
			t.Errorf("%q has no sentence; every reason must read as English on the page", r)
		}
	}
}

// --- Property 2: a stat cannot be a best case ------------------------------

func TestNewStatRefusesAnEmptySample(t *testing.T) {
	// A median of nothing is not zero, and a zero Stat is indistinguishable
	// from a real measurement of zero.
	if _, err := NewStat(nil); !errors.Is(err, ErrEmptySample) {
		t.Fatalf("NewStat(nil) = %v, want ErrEmptySample", err)
	}
	if _, err := NewStat([]float64{}); !errors.Is(err, ErrEmptySample) {
		t.Fatalf("NewStat(empty) = %v, want ErrEmptySample", err)
	}
}

func TestNewStatComputesTheOrderStatistics(t *testing.T) {
	st, err := NewStat([]float64{5, 1, 4, 2, 3, 9, 7, 6, 8, 10})
	if err != nil {
		t.Fatalf("NewStat: %v", err)
	}
	if st.N != 10 {
		t.Errorf("N = %d, want 10", st.N)
	}
	if st.Median != 5.5 {
		t.Errorf("Median = %v, want 5.5 (even N averages the two middle values)", st.Median)
	}
	if st.Min != 1 || st.Max != 10 {
		t.Errorf("Min/Max = %v/%v, want 1/10", st.Min, st.Max)
	}
	if st.P10 >= st.Median || st.P90 <= st.Median {
		t.Errorf("p10=%v p90=%v do not bracket the median %v", st.P10, st.P90, st.Median)
	}
}

func TestNewStatDoesNotMutateTheCallersSample(t *testing.T) {
	// Mutating a caller's slice from a summarising function turns up three
	// refactors later as a mysteriously reordered corpus.
	in := []float64{3, 1, 2}
	if _, err := NewStat(in); err != nil {
		t.Fatalf("NewStat: %v", err)
	}
	if in[0] != 3 || in[1] != 1 || in[2] != 2 {
		t.Fatalf("NewStat reordered the caller's slice: %v", in)
	}
}

func TestASingleSampleReportsNEqualsOneRatherThanPretendingToASpread(t *testing.T) {
	st, err := NewStat([]float64{42})
	if err != nil {
		t.Fatalf("NewStat: %v", err)
	}
	if st.N != 1 || st.Median != 42 || st.P10 != 42 || st.P90 != 42 || st.MAD != 0 {
		t.Fatalf("N=1 stat = %+v; every order statistic is the one value", st)
	}
	f := Figure{Metric: MetricProviderCalls, Unit: UnitCalls, Stat: &st, Prov: ciProv()}
	if !strings.Contains(f.Render(), "N=1") {
		t.Errorf("Render() = %q, want the N visible so nobody reads it as a population", f.Render())
	}
}

func TestEveryRenderedNumberCarriesItsSpreadAndItsN(t *testing.T) {
	// There is no mode that produces a bare median. One function is the only
	// way to keep that true of the scorecard, the page, the OS surface and the
	// CI comment at once.
	for _, sample := range [][]float64{{1}, {1, 2}, {1, 2, 3, 4, 5}, {1, 2, 3, 4, 5, 6, 7, 8, 9, 10}} {
		f, err := Measured(MetricRecoveryWallClock, sample, ciProv())
		if err != nil {
			t.Fatalf("Measured: %v", err)
		}
		out := f.Render()
		if !strings.Contains(out, "N=") {
			t.Errorf("Render() = %q for N=%d, want an N", out, len(sample))
		}
		if !strings.Contains(out, "-") {
			t.Errorf("Render() = %q for N=%d, want a spread", out, len(sample))
		}
	}
}

func TestStatHasNoMeanField(t *testing.T) {
	// Guarded by construction, asserted so that adding one is a deliberate
	// act with a test to delete: a mean is exactly the figure a best case
	// hides in.
	st, err := NewStat([]float64{1, 1, 1, 1, 100})
	if err != nil {
		t.Fatalf("NewStat: %v", err)
	}
	if st.Median != 1 {
		t.Fatalf("Median = %v, want 1: the outlier must not move it (a mean would read 20.8)", st.Median)
	}
}

// --- Property 3: provenance is mandatory, and its bar rises with the tier ---

func TestACIFigureNeedsACommitAndADate(t *testing.T) {
	for _, tc := range []struct {
		name string
		prov Provenance
		want string
	}{
		{"no commit", Provenance{Date: "2026-09-06", Tier: TierCI}, "commit"},
		{"no date", Provenance{Commit: "abc1234", Tier: TierCI}, "date"},
		{"no tier", Provenance{Commit: "abc1234", Date: "2026-09-06"}, "tier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, missing := tc.prov.Complete()
			if ok {
				t.Fatal("Complete() accepted an incomplete provenance")
			}
			if missing != tc.want {
				t.Errorf("missing = %q, want %q", missing, tc.want)
			}
		})
	}
}

func TestALiveFigureAdditionallyNeedsModelIdsAndACost(t *testing.T) {
	// The bar RISES with the tier: a live figure spent money on a named model
	// and has to say which and how much.
	base := Provenance{Commit: "abc1234", Date: "2026-09-06", Tier: TierLive}
	if ok, missing := base.Complete(); ok || missing != "modelIds" {
		t.Fatalf("Complete() = %v/%q, want false/modelIds", ok, missing)
	}
	base.ModelIds = []string{"claude-sonnet-5"}
	if ok, missing := base.Complete(); ok || missing != "costUsd" {
		t.Fatalf("Complete() = %v/%q, want false/costUsd", ok, missing)
	}
	zero := 0.0
	base.CostUSD = &zero
	if ok, missing := base.Complete(); !ok {
		t.Fatalf("a live provenance with a ZERO cost was refused for %q -- zero is a cost, absent is not", missing)
	}
}

func TestTheSameProvenanceThatPassesForCIFailsForLive(t *testing.T) {
	p := ciProv()
	if ok, _ := p.Complete(); !ok {
		t.Fatal("the CI bar rejected a complete CI provenance")
	}
	p.Tier = TierLive
	if ok, _ := p.Complete(); ok {
		t.Fatal("a CI provenance passed the live bar; the bar does not rise")
	}
}

func TestValidateRefusesAnIncompleteProvenance(t *testing.T) {
	f, err := Measured(MetricProviderCalls, []float64{0}, Provenance{Tier: TierCI, Date: "2026-09-06"})
	if err != nil {
		t.Fatalf("Measured: %v", err)
	}
	if err := f.Validate(); !errors.Is(err, ErrProvenanceIncomplete) {
		t.Fatalf("Validate() = %v, want ErrProvenanceIncomplete", err)
	}
}

// --- The metric registry ---------------------------------------------------

func TestAnUnregisteredMetricCannotBePublished(t *testing.T) {
	if _, err := Measured(Metric("made.up"), []float64{1}, ciProv()); err == nil {
		t.Fatal("Measured accepted an unregistered metric")
	}
	if _, err := Unmeasured(Metric("made.up"), ReasonTierNotRun, "", ciProv()); err == nil {
		t.Fatal("Unmeasured accepted an unregistered metric")
	}
}

func TestEveryRegisteredMetricIsCompletelySpecified(t *testing.T) {
	// Adding a metric is a deliberate act: it needs a family, a unit, a
	// direction and a sentence saying what it counts, because the scorecard
	// prints that sentence beside the number.
	for _, m := range RegisteredMetrics() {
		spec, ok := MetricSpec(m)
		if !ok {
			t.Fatalf("%s is listed but not registered", m)
		}
		if !spec.Family.Valid() {
			t.Errorf("%s has family %q, which is not one of the closed set", m, spec.Family)
		}
		if !spec.Unit.Valid() {
			t.Errorf("%s has unit %q, which is not a known unit", m, spec.Unit)
		}
		switch spec.Direction {
		case LowerIsBetter, HigherIsBetter, NeitherIsBetter:
		default:
			t.Errorf("%s has direction %q", m, spec.Direction)
		}
		if len(strings.Fields(spec.Means)) < 4 {
			t.Errorf("%s's Means is %q; it must be a sentence a reader who has never seen this suite can act on", m, spec.Means)
		}
		if !strings.HasSuffix(spec.Means, ".") {
			t.Errorf("%s's Means does not end in a full stop: %q", m, spec.Means)
		}
		if !strings.HasPrefix(string(m), string(spec.Family)+".") {
			t.Errorf("%s is registered under family %q but its name does not start with it; the page groups by the prefix", m, spec.Family)
		}
	}
}

func TestEveryGovernanceMetricBlocks(t *testing.T) {
	// Governance is pass-or-fail (design section D). There is no governance
	// score, so a governance metric that merely reported would be a property
	// nothing enforces.
	for _, m := range RegisteredMetrics() {
		spec, _ := MetricSpec(m)
		if spec.Family == FamilyGovernance && !spec.Blocking {
			t.Errorf("%s is a governance property but does not block a merge", m)
		}
	}
}

func TestNoCostOrSpeedMetricBlocks(t *testing.T) {
	// Design P2: structural properties block, cost and speed report. A cost
	// threshold reds the lane for runner noise, and the first fix anybody
	// reaches for is widening it until it means nothing -- taking the
	// structural half with it.
	for _, m := range RegisteredMetrics() {
		spec, _ := MetricSpec(m)
		if !spec.Blocking {
			continue
		}
		if spec.Unit == UnitUSD || spec.Unit == UnitTokens || spec.Unit == UnitMillis {
			t.Errorf("%s blocks a merge and is measured in %s; P2 says cost and speed report rather than block", m, spec.Unit)
		}
		if spec.Family == FamilySpeed {
			t.Errorf("%s blocks a merge and is a speed metric; P2 says speed reports", m)
		}
	}
}

// --- Comparison ------------------------------------------------------------

func TestCompareIsUndecidableAcrossEveryIncomparablePair(t *testing.T) {
	measured := func(m Metric, v float64, p Provenance) Figure {
		f, err := Measured(m, []float64{v}, p)
		if err != nil {
			t.Fatalf("Measured: %v", err)
		}
		return f
	}
	absent := func(m Metric) Figure {
		f, err := Unmeasured(m, ReasonTierNotRun, "", ciProv())
		if err != nil {
			t.Fatalf("Unmeasured: %v", err)
		}
		return f
	}

	for _, tc := range []struct {
		name          string
		before, now   Figure
		wantSubstring string
	}{
		{"different metrics", measured(MetricProviderCalls, 1, ciProv()), measured(MetricPassRate, 1, ciProv()), "different metrics"},
		{"before unmeasured", absent(MetricProviderCalls), measured(MetricProviderCalls, 1, ciProv()), "earlier figure"},
		{"now unmeasured", measured(MetricProviderCalls, 1, ciProv()), absent(MetricProviderCalls), "current figure"},
		{"different tiers", measured(MetricUSDPerGoal, 1, ciProv()), measured(MetricUSDPerGoal, 1, liveProv()), "different tiers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Compare(tc.before, tc.now)
			if d.Verdict != Undecidable {
				t.Fatalf("Verdict = %q, want undecidable", d.Verdict)
			}
			if !strings.Contains(d.Reason, tc.wantSubstring) {
				t.Errorf("Reason = %q, want it to mention %q", d.Reason, tc.wantSubstring)
			}
		})
	}
}

func TestCompareReadsTheDirectionFromTheRegistry(t *testing.T) {
	mk := func(m Metric, v float64) Figure {
		f, err := Measured(m, []float64{v}, ciProv())
		if err != nil {
			t.Fatalf("Measured: %v", err)
		}
		return f
	}
	// Lower is better for provider calls: fewer is an improvement.
	if d := Compare(mk(MetricProviderCalls, 5), mk(MetricProviderCalls, 2)); d.Verdict != Improved {
		t.Errorf("fewer provider calls read as %q, want improved", d.Verdict)
	}
	if d := Compare(mk(MetricProviderCalls, 2), mk(MetricProviderCalls, 5)); d.Verdict != Regressed {
		t.Errorf("more provider calls read as %q, want regressed", d.Verdict)
	}
	// Higher is better for a pass rate: the SAME numeric move is the other
	// verdict, which is the whole reason direction lives in the registry.
	if d := Compare(mk(MetricPassRate, 0.5), mk(MetricPassRate, 0.9)); d.Verdict != Improved {
		t.Errorf("a rising pass rate read as %q, want improved", d.Verdict)
	}
	if d := Compare(mk(MetricPassRate, 0.9), mk(MetricPassRate, 0.5)); d.Verdict != Regressed {
		t.Errorf("a falling pass rate read as %q, want regressed", d.Verdict)
	}
}

func TestCompareOmitsARelativeFigureWhenTheEarlierValueWasZero(t *testing.T) {
	// A percentage change from zero is not a percentage. Reporting one is how
	// "provider calls went from 0 to 3" becomes "+Inf%" in a table.
	mk := func(v float64) Figure {
		f, err := Measured(MetricProviderCalls, []float64{v}, ciProv())
		if err != nil {
			t.Fatalf("Measured: %v", err)
		}
		return f
	}
	d := Compare(mk(0), mk(3))
	if d.Verdict != Regressed {
		t.Fatalf("Verdict = %q, want regressed", d.Verdict)
	}
	if d.Relative != nil {
		t.Fatalf("Relative = %v, want nil when the earlier value was zero", *d.Relative)
	}
	if got := d.Render(); !strings.Contains(got, "was zero") {
		t.Errorf("Render() = %q, want it to say why there is no percentage", got)
	}
	if strings.Contains(d.Render(), "Inf") || strings.Contains(d.Render(), "NaN") {
		t.Errorf("Render() = %q, which is not a number a reader can use", d.Render())
	}
}

func TestCompareCarriesTheBlockingFlagSoTheGateNeedsNoSecondLookup(t *testing.T) {
	mk := func(m Metric, v float64) Figure {
		f, _ := Measured(m, []float64{v}, ciProv())
		return f
	}
	if d := Compare(mk(MetricDuplicatedEffects, 0), mk(MetricDuplicatedEffects, 1)); !d.Blocking {
		t.Error("a duplicated side effect does not block a merge")
	}
	if d := Compare(mk(MetricWallClockPerGoal, 100), mk(MetricWallClockPerGoal, 200)); d.Blocking {
		t.Error("a wall-clock regression blocks a merge; P2 says speed reports")
	}
}

// --- Rendering -------------------------------------------------------------

func TestRatiosRenderAsPercentagesAndDollarsAsDollars(t *testing.T) {
	for _, tc := range []struct {
		unit Unit
		in   float64
		want string
	}{
		{UnitRatio, 0.714, "71.4%"},
		{UnitUSD, 0.0412, "$0.0412"},
		{UnitMillis, 250, "250ms"},
		{UnitMillis, 2500, "2.50s"},
		{UnitCalls, 0, "0"},
		{UnitPercent, 3.1, "+3.1%"},
	} {
		if got := formatValue(tc.in, tc.unit); got != tc.want {
			t.Errorf("formatValue(%v, %s) = %q, want %q", tc.in, tc.unit, got, tc.want)
		}
	}
}

func TestPercentileInterpolatesRatherThanRounding(t *testing.T) {
	s := []float64{0, 10}
	if got := percentile(s, 0.5); got != 5 {
		t.Errorf("percentile(0.5) = %v, want 5 by interpolation", got)
	}
	if got := percentile(s, 0.1); math.Abs(got-1) > 1e-9 {
		t.Errorf("percentile(0.1) = %v, want 1", got)
	}
}
