// Package figure is the proving suite's honesty layer: a measured number,
// what it was measured on, and the vocabulary for saying that it was not
// measured at all.
//
// It is PURE -- standard library only, no engine, no database, no provider --
// and that is asserted as a build-graph fact rather than promised
// (TestProvingPureSubpackagesImportNothingBeyondStdlib in the parent package).
// The reason is the epic's own standard: the numbers must be honest before
// they are good, and a statistics layer that could reach the engine is one
// whose output nobody can check without running the engine.
//
// Three properties are enforced here rather than reviewed:
//
//  1. UNMEASURED IS A VALUE, NOT A ZERO. A Figure carries either a Stat or an
//     AbsentReason -- never both, never neither. Rendering one with neither is
//     a programming error that says so, instead of printing 0. An absent
//     figure and a zero are different answers (the rule campaignStats,
//     AiSuggestResult.usage and the OS all already follow).
//
//  2. A STAT CANNOT BE A BEST CASE. There is no Mean field and no
//     single-number constructor. NewStat refuses an empty sample. A caller who
//     wants one number gets the median and is handed the spread with it,
//     because "medians and spread, never a best case" is an epic decision and
//     a struct is a better place to keep it than a review comment.
//
//  3. PROVENANCE IS MANDATORY, AND ITS BAR RISES WITH THE TIER. A CI figure
//     needs a commit and a date; a live figure needs those plus the model ids
//     and what it cost. Render refuses a figure that cannot say where it came
//     from.
package figure

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Tier is which lane produced a figure. The two tiers answer different
// questions and their figures are NOT comparable -- see Compare, which returns
// Undecidable across a tier boundary rather than pretending.
type Tier string

const (
	// TierCI is the deterministic replay lane: no provider, every model
	// response served from a recorded cassette. It measures structure.
	TierCI Tier = "ci"
	// TierLive is the real-provider lane. It measures cost, variance and
	// wall-clock -- the things a replay cannot answer.
	TierLive Tier = "live"
)

// Valid reports whether t is one of the two tiers.
func (t Tier) Valid() bool { return t == TierCI || t == TierLive }

// Family is one of the six the epic scopes. Closed: a seventh is a design
// change, and a scenario naming one that is not here is refused at load.
type Family string

const (
	FamilyAmortizedCost Family = "amortizedCost"
	FamilyReliability   Family = "reliability"
	FamilyRecovery      Family = "recovery"
	FamilyDurability    Family = "durability"
	FamilyLearningCurve Family = "learningCurve"
	FamilySpeed         Family = "speed"
	// FamilyGovernance is the seventh name and deliberately not a "family" in
	// the epic's sense: it carries pass-or-fail properties and no statistics.
	// It is here so a governance result can be carried by the same types.
	FamilyGovernance Family = "governance"
)

// AllFamilies returns the closed set, in the order the scorecard prints them.
func AllFamilies() []Family {
	return []Family{
		FamilyAmortizedCost, FamilyReliability, FamilyRecovery,
		FamilyDurability, FamilyLearningCurve, FamilySpeed, FamilyGovernance,
	}
}

// Valid reports whether f is one of the seven.
func (f Family) Valid() bool {
	for _, k := range AllFamilies() {
		if f == k {
			return true
		}
	}
	return false
}

// Arm is what was under test. Exactly two, and there will not be a third: the
// epic's comparison is "a model by itself versus the model with MemQL", and a
// third arm turns a comparison into a survey.
type Arm string

const (
	// ArmPlatform is the work spine with its machinery on.
	ArmPlatform Arm = "platform"
	// ArmBaseline is the same model, the same tools, the same scenario, in a
	// plain bounded tool loop with the platform's machinery switched off.
	ArmBaseline Arm = "baseline"
)

// Valid reports whether a is one of the two arms.
func (a Arm) Valid() bool { return a == ArmPlatform || a == ArmBaseline }

// Unit is what a figure counts. Closed, because the unit decides how a figure
// renders and whether two figures may be compared at all.
type Unit string

const (
	UnitCalls   Unit = "calls"
	UnitSteps   Unit = "steps"
	UnitTokens  Unit = "tokens"
	UnitUSD     Unit = "usd"
	UnitMillis  Unit = "ms"
	UnitRatio   Unit = "ratio"   // 0..1, rendered as a percentage
	UnitPercent Unit = "percent" // a percentage CHANGE, which ratio is not
	UnitCount   Unit = "count"
)

// Valid reports whether u is a known unit.
func (u Unit) Valid() bool {
	switch u {
	case UnitCalls, UnitSteps, UnitTokens, UnitUSD, UnitMillis, UnitRatio, UnitPercent, UnitCount:
		return true
	}
	return false
}

// Direction says which way is better for a metric. Declared once, in the
// metric table, because "lower is better" is true of cost and false of pass
// rate and a comparison that guessed would report every improvement backwards
// half the time.
type Direction string

const (
	// LowerIsBetter: cost, calls, wall-clock, duplicated effects.
	LowerIsBetter Direction = "lower"
	// HigherIsBetter: pass rate, catalog-served fraction, recovery rate.
	HigherIsBetter Direction = "higher"
	// NeitherIsBetter: a figure that is descriptive rather than a score --
	// N, a step count, a token count reported for context. A delta over one of
	// these is reported as a change and never as an improvement.
	NeitherIsBetter Direction = "neither"
)

// Metric names one measurable thing. It is a string rather than an enum so a
// scenario may declare a metric of its own, but every metric the suite
// PUBLISHES must be registered in Metrics below -- an unregistered metric has
// no unit and no direction, so nothing can render or compare it, and
// MetricSpec returns ok=false rather than guessing.
type Metric string

// The registered metrics. Adding one is a deliberate act: it needs a family,
// a unit, a direction and a sentence saying what it means, and the scorecard
// prints that sentence beside the number.
const (
	// Amortized cost.
	MetricProviderCalls     Metric = "amortizedCost.providerCalls"
	MetricStepsServed       Metric = "amortizedCost.stepsServedFromJournal"
	MetricTokensPerGoal     Metric = "amortizedCost.tokensPerGoal"
	MetricUSDPerGoal        Metric = "amortizedCost.usdPerGoal"
	MetricCompileCallsExact Metric = "amortizedCost.compileCallsOnCatalogHit"

	// Reliability.
	MetricPassRate     Metric = "reliability.passRate"
	MetricPassVariance Metric = "reliability.passVariance"

	// Recovery.
	MetricRecoveryRate      Metric = "recovery.rate"
	MetricRecoveryCalls     Metric = "recovery.modelCalls"
	MetricStepsReExecuted   Metric = "recovery.stepsReExecuted"
	MetricRepairVsRestart   Metric = "recovery.repairStepsVsRestartSteps"
	MetricRecoveryWallClock Metric = "recovery.wallClockMs"

	// Durability.
	MetricDuplicatedEffects Metric = "durability.duplicatedSideEffects"
	MetricResumedElsewhere  Metric = "durability.resumedOnAnotherNode"
	MetricResumeReExecuted  Metric = "durability.resumedStepsReExecuted"

	// Learning curve.
	MetricCatalogServedFraction Metric = "learningCurve.catalogServedFraction"
	MetricUSDPerGoalInSequence  Metric = "learningCurve.usdPerGoal"

	// Speed.
	//
	// The journal figure is an ABSOLUTE per-step cost, not a ratio. The first
	// version was a ratio against the same steps unjournaled and it published
	// 12,907: a speed scenario's steps are deliberately trivial, so the
	// journal is essentially all of the time and the denominator is
	// degenerate. The arithmetic was right and the figure meant nothing.
	// "The journal's own per-step overhead" is milliseconds per step, and
	// that is a number a reader can act on whatever the steps do.
	MetricJournalOverhead  Metric = "speed.journalPerStepOverheadMs"
	MetricWallClockPerGoal Metric = "speed.wallClockPerGoalMs"
	MetricReplaySpeedup    Metric = "speed.firstRunVsReplayRatio"

	// Governance -- pass-or-fail, carried as a ratio that must be exactly 1.
	MetricEffectsHaveReceipts Metric = "governance.effectsWithReceipts"
	MetricModelCallsJournaled Metric = "governance.modelCallsJournaled"
	MetricApprovalsHashBound  Metric = "governance.approvalsBoundToArtifactHash"
)

// Spec is what the registry knows about a metric.
type Spec struct {
	Metric    Metric
	Family    Family
	Unit      Unit
	Direction Direction
	// Means is printed beside the number on the generated page. It is a
	// sentence, not a label: a reader who has never seen this suite should be
	// able to tell what was counted.
	Means string
	// Blocking marks a metric whose regression FAILS a pull request (design
	// P2). Structural properties block; cost and speed report.
	Blocking bool
}

var metrics = map[Metric]Spec{
	MetricProviderCalls:     {MetricProviderCalls, FamilyAmortizedCost, UnitCalls, LowerIsBetter, "Provider calls one run made. A run served entirely from the journal makes none.", true},
	MetricStepsServed:       {MetricStepsServed, FamilyAmortizedCost, UnitSteps, HigherIsBetter, "Steps answered from the journal rather than re-executed.", false},
	MetricTokensPerGoal:     {MetricTokensPerGoal, FamilyAmortizedCost, UnitTokens, LowerIsBetter, "Tokens spent per goal, amortized over N runs with different variables.", false},
	MetricUSDPerGoal:        {MetricUSDPerGoal, FamilyAmortizedCost, UnitUSD, LowerIsBetter, "Dollars per goal, amortized over N runs with different variables.", false},
	MetricCompileCallsExact: {MetricCompileCallsExact, FamilyAmortizedCost, UnitCalls, LowerIsBetter, "Model calls made while compiling a goal that exactly matches the catalog.", true},

	MetricPassRate:     {MetricPassRate, FamilyReliability, UnitRatio, HigherIsBetter, "Runs whose verifier passed, over runs attempted.", true},
	MetricPassVariance: {MetricPassVariance, FamilyReliability, UnitRatio, LowerIsBetter, "Spread of the pass rate across K runs of one goal.", false},

	MetricRecoveryRate:      {MetricRecoveryRate, FamilyRecovery, UnitRatio, HigherIsBetter, "Runs that reached a passing verifier after an injected failure.", true},
	MetricRecoveryCalls:     {MetricRecoveryCalls, FamilyRecovery, UnitCalls, LowerIsBetter, "Model calls spent recovering from an injected failure.", false},
	MetricStepsReExecuted:   {MetricStepsReExecuted, FamilyRecovery, UnitSteps, LowerIsBetter, "Already-completed steps re-executed while recovering. Repair from the failed step re-executes none.", true},
	MetricRepairVsRestart:   {MetricRepairVsRestart, FamilyRecovery, UnitRatio, LowerIsBetter, "Steps a repair ran, over steps a restart from the beginning would have run.", false},
	MetricRecoveryWallClock: {MetricRecoveryWallClock, FamilyRecovery, UnitMillis, LowerIsBetter, "Wall-clock from the injected failure to a passing verifier.", false},

	MetricDuplicatedEffects: {MetricDuplicatedEffects, FamilyDurability, UnitCount, LowerIsBetter, "Side effects delivered twice across a mid-run kill and a resume. Must be zero.", true},
	MetricResumedElsewhere:  {MetricResumedElsewhere, FamilyDurability, UnitRatio, HigherIsBetter, "Killed runs that resumed on a different node from the journal alone.", true},
	MetricResumeReExecuted:  {MetricResumeReExecuted, FamilyDurability, UnitSteps, LowerIsBetter, "Completed steps a resumed run executed again. Must be zero.", true},

	MetricCatalogServedFraction: {MetricCatalogServedFraction, FamilyLearningCurve, UnitRatio, HigherIsBetter, "Steps served by the catalog, across a sequence of related goals.", false},
	MetricUSDPerGoalInSequence:  {MetricUSDPerGoalInSequence, FamilyLearningCurve, UnitUSD, LowerIsBetter, "Dollars per goal as the catalog fills across a related sequence.", false},

	MetricJournalOverhead:  {MetricJournalOverhead, FamilySpeed, UnitMillis, LowerIsBetter, "Milliseconds the journal itself adds per step, measured in one process against the same steps unjournaled.", false},
	MetricWallClockPerGoal: {MetricWallClockPerGoal, FamilySpeed, UnitMillis, LowerIsBetter, "Wall-clock for one goal, end to end.", false},
	MetricReplaySpeedup:    {MetricReplaySpeedup, FamilySpeed, UnitRatio, LowerIsBetter, "A replayed run's wall-clock over the first run's.", false},

	MetricEffectsHaveReceipts: {MetricEffectsHaveReceipts, FamilyGovernance, UnitRatio, HigherIsBetter, "Side-effecting steps that left a receipt. Must be 1.", true},
	MetricModelCallsJournaled: {MetricModelCallsJournaled, FamilyGovernance, UnitRatio, HigherIsBetter, "Model calls the run journaled, over model calls it made. Must be 1.", true},
	MetricApprovalsHashBound:  {MetricApprovalsHashBound, FamilyGovernance, UnitRatio, HigherIsBetter, "Approvals carrying the hash of the artifact approved. Must be 1.", true},
}

// MetricSpec returns the registration for m. ok is false for an unregistered
// metric, and every caller that renders or compares MUST branch on it: an
// unregistered metric has no direction, so a comparison would have to guess
// which way is better, and guessing wrong reports a regression as a win.
func MetricSpec(m Metric) (Spec, bool) {
	s, ok := metrics[m]
	return s, ok
}

// RegisteredMetrics returns every registered metric, sorted, so the scorecard
// and the generated page walk them in a stable order.
func RegisteredMetrics() []Metric {
	out := make([]Metric, 0, len(metrics))
	for m := range metrics {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AbsentReason says why a figure has no number. CLOSED on purpose: a
// free-text reason drifts into "n/a" within two releases and stops meaning
// anything, and the whole value of an absent figure is that it says something
// specific.
type AbsentReason string

const (
	// ReasonNotMeasurableOnReplay -- deterministic by construction. A replay
	// has no variance and its wall-clock is the runner's, not the product's.
	// Only the live tier can answer.
	ReasonNotMeasurableOnReplay AbsentReason = "notMeasurableOnReplay"
	// ReasonSeamNotBuilt -- the code that would produce this does not exist
	// yet. Detail names it, and Unmeasured REFUSES a blank one.
	//
	// It carried the work spine's two unbuilt seams until memql#4999 built
	// them, and reporting zero provider calls because nothing calls a
	// provider would have been a lie that reads exactly like the headline
	// result. No figure uses it today; it stays in the set because the next
	// unbuilt seam should reach for it rather than invent a reason.
	ReasonSeamNotBuilt AbsentReason = "seamNotBuilt"
	// ReasonTierNotRun -- this tier has not been dispatched. The normal state
	// of every live figure while the live lane is disarmed.
	ReasonTierNotRun AbsentReason = "tierNotRun"
	// ReasonNoProvider -- the lane ran and self-skipped for want of a
	// credential.
	ReasonNoProvider AbsentReason = "noProvider"
	// ReasonBelowFloor -- fewer samples than the family's declared floor. A
	// median of two is not a median.
	ReasonBelowFloor AbsentReason = "belowFloor"
	// ReasonCeilingReached -- the spend ceiling stopped the run before this
	// figure was complete. A published result, not a crash.
	ReasonCeilingReached AbsentReason = "ceilingReached"
)

// AllAbsentReasons returns the closed set.
func AllAbsentReasons() []AbsentReason {
	return []AbsentReason{
		ReasonNotMeasurableOnReplay, ReasonSeamNotBuilt, ReasonTierNotRun,
		ReasonNoProvider, ReasonBelowFloor, ReasonCeilingReached,
	}
}

// Valid reports whether r is one of the closed set.
func (r AbsentReason) Valid() bool {
	for _, k := range AllAbsentReasons() {
		if r == k {
			return true
		}
	}
	return false
}

// Sentence is the reason in words, for the generated page and the OS surface.
func (r AbsentReason) Sentence() string {
	switch r {
	case ReasonNotMeasurableOnReplay:
		return "not measurable on a replay -- a replay is deterministic, so only the live tier can answer this"
	case ReasonSeamNotBuilt:
		return "the code that would produce this is not built yet"
	case ReasonTierNotRun:
		return "this tier has not been run"
	case ReasonNoProvider:
		return "the run self-skipped: no provider credential was configured"
	case ReasonBelowFloor:
		return "too few samples to state a median"
	case ReasonCeilingReached:
		return "the spend ceiling stopped the run before this was complete"
	}
	return string(r)
}

var (
	// ErrEmptySample is returned by NewStat for a sample of length zero. A
	// median of nothing is not zero.
	ErrEmptySample = errors.New("proving/figure: a statistic needs at least one sample")
	// ErrNotAFigure is returned when a value is neither measured nor
	// explicitly absent. It is a programming error in the runner, and it is
	// loud because the alternative is printing a zero.
	ErrNotAFigure = errors.New("proving/figure: a figure carries either a statistic or an absent reason, and this carries neither")
	// ErrBothStatAndReason is its mirror.
	ErrBothStatAndReason = errors.New("proving/figure: a figure carries either a statistic or an absent reason, and this carries both")
	// ErrProvenanceIncomplete is returned when a figure cannot say where it
	// came from at the standard its tier requires.
	ErrProvenanceIncomplete = errors.New("proving/figure: provenance is incomplete for this tier")
)

// Stat is a sample summarised. There is deliberately NO Mean field and no
// constructor that takes a single number: "medians and spread, never a best
// case" is an epic decision, and a type is a better place to keep a decision
// than a review comment.
type Stat struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
	P10    float64 `json:"p10"`
	P90    float64 `json:"p90"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	// MAD is the median absolute deviation. It is the spread figure the
	// scorecard quotes for a small N, because a p10/p90 over five samples is
	// two order statistics wearing a percentile's name.
	MAD float64 `json:"mad"`
}

// NewStat summarises a sample. It refuses an empty one rather than returning
// a zero Stat, because a zero Stat is indistinguishable from a real
// measurement of zero and the whole point of this package is that those two
// are different answers.
//
// The input is not modified: percentiles need a sorted copy and mutating a
// caller's slice from a summarising function is the kind of surprise that
// turns up three refactors later as a mysteriously reordered corpus.
func NewStat(sample []float64) (Stat, error) {
	if len(sample) == 0 {
		return Stat{}, ErrEmptySample
	}
	s := make([]float64, len(sample))
	copy(s, sample)
	sort.Float64s(s)

	med := median(s)
	dev := make([]float64, len(s))
	for i, v := range s {
		dev[i] = math.Abs(v - med)
	}
	sort.Float64s(dev)

	return Stat{
		N:      len(s),
		Median: med,
		P10:    percentile(s, 0.10),
		P90:    percentile(s, 0.90),
		Min:    s[0],
		Max:    s[len(s)-1],
		MAD:    median(dev),
	}, nil
}

// median of an already-sorted slice. Even N averages the two middle values,
// which is the usual definition and matters at N=2 where the alternative
// (take the lower) would bias every small sample downward.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// percentile of an already-sorted slice, by linear interpolation between the
// two nearest ranks. At N=1 every percentile is the one value, which is
// correct and reads honestly beside N=1.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	pos := p * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// Provenance is where a figure came from. Every field on it is something a
// reader needs in order to decide whether the number still applies, which is
// why the epic names them: N and spread come from Stat, and commit, date,
// model ids and cost come from here.
type Provenance struct {
	// Commit is the short SHA the suite ran at. Required in every tier.
	Commit string `json:"commit"`
	// Date is the day the figure was produced, YYYY-MM-DD. Required in every
	// tier. A date rather than a timestamp: the scorecard is a daily
	// artifact, and a timestamp invites reading precision into it that the
	// weekly refresh cadence does not support.
	Date string `json:"date"`
	// Tier is which lane produced it.
	Tier Tier `json:"tier"`
	// ModelIds are the models involved, sorted. Required for a LIVE figure and
	// meaningless for a CI one, where the models are whatever the cassette
	// recorded and are carried on the cassette instead.
	ModelIds []string `json:"modelIds,omitempty"`
	// CostUSD is what producing this figure cost. Required for a LIVE figure;
	// a CI figure costs nothing and says so by leaving it nil rather than
	// writing 0, because 0 and "no provider was reached" are different claims
	// even about money.
	CostUSD *float64 `json:"costUsd,omitempty"`
	// Runner names the machine class, so a wall-clock figure from a shared CI
	// runner is not silently compared against one from a workstation.
	Runner string `json:"runner,omitempty"`
}

// Complete reports whether the provenance meets its tier's bar, and names the
// first missing field if not. The bar RISES with the tier: a CI figure needs
// to say when and at what commit; a live figure additionally spent money on a
// named model and has to say which and how much.
func (p Provenance) Complete() (bool, string) {
	if !p.Tier.Valid() {
		return false, "tier"
	}
	if strings.TrimSpace(p.Commit) == "" {
		return false, "commit"
	}
	if strings.TrimSpace(p.Date) == "" {
		return false, "date"
	}
	if p.Tier == TierLive {
		if len(p.ModelIds) == 0 {
			return false, "modelIds"
		}
		if p.CostUSD == nil {
			return false, "costUsd"
		}
	}
	return true, ""
}

// Figure is one published number, or one published absence.
//
// The invariant -- exactly one of Stat and Absent is set -- is what makes
// "an absent figure and a zero are different answers" true of every consumer
// at once, rather than of whichever consumer remembered.
type Figure struct {
	Metric Metric       `json:"metric"`
	Unit   Unit         `json:"unit"`
	Stat   *Stat        `json:"stat,omitempty"`
	Absent AbsentReason `json:"absent,omitempty"`
	// Detail qualifies an AbsentReason. For ReasonSeamNotBuilt it NAMES the
	// missing code, which is what turns "we cannot measure this" from an
	// excuse into a work item.
	Detail string     `json:"detail,omitempty"`
	Prov   Provenance `json:"provenance"`
}

// Measured builds a figure from a sample. It is the only way to get a Figure
// with a Stat, so there is no path that produces one without an N.
func Measured(m Metric, sample []float64, prov Provenance) (Figure, error) {
	spec, ok := MetricSpec(m)
	if !ok {
		return Figure{}, fmt.Errorf("proving/figure: %q is not a registered metric; register it in metrics with a family, unit, direction and a sentence saying what it counts", m)
	}
	st, err := NewStat(sample)
	if err != nil {
		return Figure{}, fmt.Errorf("proving/figure: %s: %w", m, err)
	}
	return Figure{Metric: m, Unit: spec.Unit, Stat: &st, Prov: prov}, nil
}

// Unmeasured builds a figure that says why there is no number. detail is
// required for ReasonSeamNotBuilt -- "the code is not built" without naming
// the code is exactly the sort of absence that never becomes a work item.
func Unmeasured(m Metric, reason AbsentReason, detail string, prov Provenance) (Figure, error) {
	spec, ok := MetricSpec(m)
	if !ok {
		return Figure{}, fmt.Errorf("proving/figure: %q is not a registered metric", m)
	}
	if !reason.Valid() {
		return Figure{}, fmt.Errorf("proving/figure: %q is not a known absent reason; the set is closed (%v) so that an absence keeps meaning something", reason, AllAbsentReasons())
	}
	if reason == ReasonSeamNotBuilt && strings.TrimSpace(detail) == "" {
		return Figure{}, fmt.Errorf("proving/figure: %s is unmeasured because a seam is not built, so detail must NAME the missing code", m)
	}
	return Figure{Metric: m, Unit: spec.Unit, Absent: reason, Detail: detail, Prov: prov}, nil
}

// Validate checks the invariant and the provenance bar. Every writer calls it
// before publishing; Render calls it too, so there is no way to print a figure
// that would not survive validation.
func (f Figure) Validate() error {
	if f.Stat != nil && f.Absent != "" {
		return fmt.Errorf("%w: %s", ErrBothStatAndReason, f.Metric)
	}
	if f.Stat == nil && f.Absent == "" {
		return fmt.Errorf("%w: %s", ErrNotAFigure, f.Metric)
	}
	if f.Absent != "" && !f.Absent.Valid() {
		return fmt.Errorf("proving/figure: %s carries an unknown absent reason %q", f.Metric, f.Absent)
	}
	if ok, missing := f.Prov.Complete(); !ok {
		return fmt.Errorf("%w: %s has no %s (tier %q)", ErrProvenanceIncomplete, f.Metric, missing, f.Prov.Tier)
	}
	return nil
}

// IsMeasured reports whether there is a number.
func (f Figure) IsMeasured() bool { return f.Stat != nil }

// Render is the ONE place a figure becomes text. There is no mode that
// produces a bare median: every rendered number carries its spread and its N,
// because that is the epic's honesty rule and one function is the only way to
// keep it true of the scorecard, the page, the OS surface and the CI comment
// at the same time.
//
// An invalid figure renders its error rather than a number. A caller that
// wants to fail instead should call Validate first; a caller that is printing
// a report wants the report to say what is wrong with it, in the place the
// number would have been.
func (f Figure) Render() string {
	if err := f.Validate(); err != nil {
		return "[" + err.Error() + "]"
	}
	if f.Stat == nil {
		// THE DETAIL WINS WHEN THERE IS ONE (memql#4999). The reason's own
		// sentence is a category, and a category can be right about WHO can
		// answer while explaining the wrong mechanism: governance's
		// modelCallsJournaled reads notMeasurableOnReplay because only the
		// live tier can count it, but the reason a replay cannot is that this
		// harness plays model responses from a cassette -- not that a replay
		// is deterministic, which is what the sentence says. The detail is
		// where the figure said something specific, and it was going
		// unprinted.
		if d := strings.TrimSpace(f.Detail); d != "" {
			return "-- (" + d + ")"
		}
		return "-- (" + f.Absent.Sentence() + ")"
	}
	s := *f.Stat
	body := formatValue(s.Median, f.Unit)
	spread := formatValue(s.P10, f.Unit) + "-" + formatValue(s.P90, f.Unit)
	if s.N < 5 {
		// Below five samples a p10/p90 is two order statistics wearing a
		// percentile's name. Quote the range and the MAD instead, and say N so
		// nobody has to infer it.
		spread = formatValue(s.Min, f.Unit) + "-" + formatValue(s.Max, f.Unit)
	}
	return fmt.Sprintf("%s (%s, N=%d)", body, spread, s.N)
}

// formatValue renders one number in its unit. Ratios become percentages
// because a reader reads 0.71 as a probability and 71% as a fraction of
// something, and every ratio here is the latter.
func formatValue(v float64, u Unit) string {
	switch u {
	case UnitUSD:
		return fmt.Sprintf("$%.4f", v)
	case UnitRatio:
		return fmt.Sprintf("%.1f%%", v*100)
	case UnitPercent:
		return fmt.Sprintf("%+.1f%%", v)
	case UnitMillis:
		if v >= 1000 {
			return fmt.Sprintf("%.2fs", v/1000)
		}
		return fmt.Sprintf("%.0fms", v)
	case UnitTokens, UnitCalls, UnitSteps, UnitCount:
		if v == math.Trunc(v) {
			return fmt.Sprintf("%.0f", v)
		}
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.4g", v)
}

// Verdict is the answer to "did this get better".
type Verdict string

const (
	Improved    Verdict = "improved"
	Regressed   Verdict = "regressed"
	Unchanged   Verdict = "unchanged"
	Undecidable Verdict = "undecidable"
)

// Delta is a comparison of two figures. Undecidable is the DEFAULT and is
// returned for every case where the two are not actually comparable -- either
// side unmeasured, different units, different metrics, different tiers, or an
// unregistered metric with no declared direction. A comparison that guessed
// would report a regression as a win roughly half the time.
type Delta struct {
	Metric   Metric  `json:"metric"`
	Verdict  Verdict `json:"verdict"`
	Reason   string  `json:"reason,omitempty"`
	Absolute float64 `json:"absolute,omitempty"`
	// Relative is the fractional change, present only when the baseline is
	// non-zero. A percentage change from zero is not a percentage.
	Relative *float64 `json:"relative,omitempty"`
	// Blocking is the metric's own Blocking flag, copied here so the gate can
	// decide without a second lookup. Design P2: structural properties block,
	// cost and speed report.
	Blocking bool `json:"blocking"`
}

// Compare answers whether now is better than before. The argument order reads
// as it is written: Compare(before, now).
func Compare(before, now Figure) Delta {
	d := Delta{Metric: now.Metric}
	spec, ok := MetricSpec(now.Metric)
	if ok {
		d.Blocking = spec.Blocking
	}

	switch {
	case before.Metric != now.Metric:
		d.Verdict, d.Reason = Undecidable, fmt.Sprintf("different metrics: %s and %s", before.Metric, now.Metric)
		return d
	case !ok:
		d.Verdict, d.Reason = Undecidable, fmt.Sprintf("%s is not registered, so no direction is declared for it", now.Metric)
		return d
	case before.Unit != now.Unit:
		d.Verdict, d.Reason = Undecidable, fmt.Sprintf("different units: %s and %s", before.Unit, now.Unit)
		return d
	case before.Prov.Tier != now.Prov.Tier:
		// A live figure against a CI figure is not a comparison: the two
		// tiers measure different things about the same name.
		d.Verdict, d.Reason = Undecidable, fmt.Sprintf("different tiers: %s and %s", before.Prov.Tier, now.Prov.Tier)
		return d
	case !before.IsMeasured():
		d.Verdict, d.Reason = Undecidable, "the earlier figure is "+string(before.Absent)
		return d
	case !now.IsMeasured():
		d.Verdict, d.Reason = Undecidable, "the current figure is "+string(now.Absent)
		return d
	}

	d.Absolute = now.Stat.Median - before.Stat.Median
	if before.Stat.Median != 0 {
		rel := d.Absolute / math.Abs(before.Stat.Median)
		d.Relative = &rel
	}

	if d.Absolute == 0 {
		d.Verdict = Unchanged
		return d
	}
	switch spec.Direction {
	case NeitherIsBetter:
		d.Verdict, d.Reason = Undecidable, "this metric is descriptive: it has no better direction"
	case LowerIsBetter:
		if d.Absolute < 0 {
			d.Verdict = Improved
		} else {
			d.Verdict = Regressed
		}
	case HigherIsBetter:
		if d.Absolute > 0 {
			d.Verdict = Improved
		} else {
			d.Verdict = Regressed
		}
	default:
		d.Verdict, d.Reason = Undecidable, fmt.Sprintf("unknown direction %q", spec.Direction)
	}
	return d
}

// Render describes the delta in words, in the unit's own terms.
func (d Delta) Render() string {
	switch d.Verdict {
	case Undecidable:
		if d.Reason != "" {
			return "undecidable (" + d.Reason + ")"
		}
		return "undecidable"
	case Unchanged:
		return "unchanged"
	}
	if d.Relative == nil {
		return fmt.Sprintf("%s (%+.4g absolute; no relative figure, the earlier value was zero)", d.Verdict, d.Absolute)
	}
	return fmt.Sprintf("%s (%+.1f%%)", d.Verdict, *d.Relative*100)
}
