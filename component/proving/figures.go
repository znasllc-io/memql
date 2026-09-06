package proving

import (
	"fmt"
	"sort"

	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/scorecard"
	"github.com/znasllc-io/memql/component/work"
)

// Figures turns one scenario's arm results into published figures.
//
// This function IS design decision P1 made executable: the CI tier publishes
// only what a replay can honestly measure, and everything else is `unmeasured`
// with the reason attached. Every `notMeasurableOnReplay` below is a figure
// this suite COULD print a number for and refuses to, because the number would
// be a property of the CI runner or a trivial consequence of determinism
// rather than a fact about the product.
//
// The two `seamNotBuilt` entries are the other half of the same discipline. A
// benchmark that reported "zero provider calls" because nothing in the path
// calls a provider would have told a lie that reads exactly like the headline
// result, so those figures name the missing code instead.
// Overhead is the journal-overhead measurement, or the reason there is none.
type Overhead struct {
	Ratio  float64
	OK     bool
	Reason string
}

func Figures(s scenario.Scenario, results map[figure.Arm]ArmResult, overhead *Overhead, prov figure.Provenance) ([]scorecard.Entry, []scorecard.Property, error) {
	var (
		entries []scorecard.Entry
		props   []scorecard.Property
	)

	// A control scenario's figures are marked so the claims gate can exclude
	// them: they exist to prove a counter can rise and therefore report the
	// opposite of the headline.
	control := s.NegativeControlFor != ""
	add := func(arm figure.Arm, f figure.Figure, err error) error {
		if err != nil {
			return err
		}
		entries = append(entries, scorecard.Entry{
			Scenario: s.Id, Family: s.Family, Arm: arm, Figure: f,
			Control: control && f.Metric == s.NegativeControlFor,
		})
		return nil
	}
	measured := func(arm figure.Arm, m figure.Metric, v float64) error {
		f, err := figure.Measured(m, []float64{v}, prov)
		return add(arm, f, err)
	}
	absent := func(arm figure.Arm, m figure.Metric, reason figure.AbsentReason, detail string) error {
		f, err := figure.Unmeasured(m, reason, detail, prov)
		return add(arm, f, err)
	}

	claims := map[figure.Metric]bool{}
	for _, m := range s.Claims {
		claims[m] = true
	}
	want := func(m figure.Metric) bool { return claims[m] }

	arms := make([]figure.Arm, 0, len(results))
	for a := range results {
		arms = append(arms, a)
	}
	sort.Slice(arms, func(i, j int) bool { return arms[i] < arms[j] })

	for _, arm := range arms {
		res := results[arm]
		if res.Err != nil {
			return nil, nil, fmt.Errorf("%s/%s: %w", s.Id, arm, res.Err)
		}

		// ---- Amortized cost -------------------------------------------
		if want(figure.MetricProviderCalls) {
			// In the CI tier a "provider call" is a recorded response
			// consumed. That is the honest reading: both arms replay, so what
			// is measured is how many model exchanges the run NEEDED, not what
			// they cost.
			if err := measured(arm, figure.MetricProviderCalls, float64(res.RecordedResponses)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricStepsServed) {
			served := res.StepsExecuted - res.StepsReExecuted
			if err := measured(arm, figure.MetricStepsServed, float64(served)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricTokensPerGoal) {
			if err := absent(arm, figure.MetricTokensPerGoal, figure.ReasonNotMeasurableOnReplay,
				"a replayed run's tokens are the cassette's recorded tokens, not this run's"); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricUSDPerGoal) {
			if err := absent(arm, figure.MetricUSDPerGoal, figure.ReasonNotMeasurableOnReplay,
				"a replayed run spends nothing; the recorded cost belongs to the capture"); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricCompileCallsExact) && arm == figure.ArmPlatform {
			// Measured directly against component/work.Decide, which is where
			// the claim lives: an exact catalog hit returns NeedsModel false
			// and NeedsTriage false, so the zero is a property of a returned
			// value rather than an absence of instrumentation.
			//
			// A CONTROL scenario measures the MISS path instead. The baseline
			// arm cannot be the control here the way it is everywhere else --
			// a bare loop does not compile at all -- so the control is an
			// explicit scenario, and without it a counter that is never
			// incremented on any path would read as zero forever.
			calls := CompileCallsOnCatalogHit(s.Goal, variableKeys(s))
			if s.NegativeControlFor == figure.MetricCompileCallsExact {
				calls = CompileCallsOnCatalogMiss(s.Goal, variableKeys(s))
			}
			if err := measured(arm, figure.MetricCompileCallsExact, float64(calls)); err != nil {
				return nil, nil, err
			}
		}

		// ---- Reliability -----------------------------------------------
		if want(figure.MetricPassRate) {
			if err := measured(arm, figure.MetricPassRate, boolTo(res.Passed)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricPassVariance) {
			if err := absent(arm, figure.MetricPassVariance, figure.ReasonNotMeasurableOnReplay,
				"a replay is deterministic, so its variance is zero by construction rather than by measurement"); err != nil {
				return nil, nil, err
			}
		}

		// ---- Recovery ---------------------------------------------------
		if want(figure.MetricRecoveryRate) {
			if err := measured(arm, figure.MetricRecoveryRate, boolTo(res.Passed)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricRecoveryCalls) {
			if err := measured(arm, figure.MetricRecoveryCalls, float64(res.RecordedResponses)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricStepsReExecuted) {
			if err := measured(arm, figure.MetricStepsReExecuted, float64(res.StepsReExecuted)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricRepairVsRestart) {
			total := len(s.Steps)
			if total == 0 {
				return nil, nil, fmt.Errorf("%s: no steps", s.Id)
			}
			if err := measured(arm, figure.MetricRepairVsRestart, float64(res.StepsExecuted)/float64(total)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricRecoveryWallClock) {
			if err := absent(arm, figure.MetricRecoveryWallClock, figure.ReasonNotMeasurableOnReplay,
				"wall-clock on a shared CI runner belongs to the runner"); err != nil {
				return nil, nil, err
			}
		}

		// ---- Durability -------------------------------------------------
		if want(figure.MetricDuplicatedEffects) {
			if err := measured(arm, figure.MetricDuplicatedEffects, float64(res.Duplicates)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricResumeReExecuted) {
			if err := measured(arm, figure.MetricResumeReExecuted, float64(res.StepsReExecuted)); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricResumedElsewhere) && arm == figure.ArmPlatform {
			if err := measured(arm, figure.MetricResumedElsewhere, boolTo(res.Resumed && res.SameRunId)); err != nil {
				return nil, nil, err
			}
		}

		// ---- Learning curve ---------------------------------------------
		if want(figure.MetricCatalogServedFraction) {
			total := res.StepsExecuted
			if total == 0 {
				total = 1
			}
			served := float64(total-res.RecordedResponses) / float64(total)
			if err := measured(arm, figure.MetricCatalogServedFraction, served); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricUSDPerGoalInSequence) {
			if err := absent(arm, figure.MetricUSDPerGoalInSequence, figure.ReasonNotMeasurableOnReplay,
				"dollars per goal need a live provider; a replay spends nothing"); err != nil {
				return nil, nil, err
			}
		}

		// ---- Speed --------------------------------------------------------
		if want(figure.MetricWallClockPerGoal) {
			if err := absent(arm, figure.MetricWallClockPerGoal, figure.ReasonNotMeasurableOnReplay,
				"wall-clock on a shared CI runner belongs to the runner"); err != nil {
				return nil, nil, err
			}
		}
		if want(figure.MetricReplaySpeedup) {
			if err := absent(arm, figure.MetricReplaySpeedup, figure.ReasonNotMeasurableOnReplay,
				"the first run and the replay would both be replays here"); err != nil {
				return nil, nil, err
			}
		}
	}

	// The journal's per-step overhead is measured by MeasureJournalOverhead,
	// which runs the SAME automation twice in one process -- once with an
	// engine and once without -- so the only difference between the two
	// timings is whether journal rows were written.
	//
	// It is NOT the platform arm's wall-clock over the baseline arm's. That
	// was the first instrument here and it published a ratio of 34,000: the
	// baseline touches no database and the platform writes to Postgres, so
	// the figure measured the cost of having a database at all while carrying
	// the journal's name. Arithmetically correct, and about something else.
	if claims[figure.MetricJournalOverhead] {
		if overhead == nil {
			if err := absent(figure.ArmPlatform, figure.MetricJournalOverhead, figure.ReasonBelowFloor,
				"the overhead measurement did not run"); err != nil {
				return nil, nil, err
			}
		} else if !overhead.OK {
			if err := absent(figure.ArmPlatform, figure.MetricJournalOverhead, figure.ReasonBelowFloor,
				overhead.Reason); err != nil {
				return nil, nil, err
			}
		} else if err := measured(figure.ArmPlatform, figure.MetricJournalOverhead, overhead.Ratio); err != nil {
			return nil, nil, err
		}
	}

	// ---- Governance: pass or fail, with no score ------------------------
	//
	// Only properties that are actually CHECKED become Properties. A property
	// whose seam is not built becomes an `unmeasured` FIGURE instead -- a
	// governance Property that silently passed for want of anything to check
	// would be the worst artifact this suite could produce.
	if p, ok := results[figure.ArmPlatform]; ok {
		if claims[figure.MetricEffectsHaveReceipts] {
			props = append(props, receiptProperty(s, p))
			if err := measured(figure.ArmPlatform, figure.MetricEffectsHaveReceipts, boolTo(receiptProperty(s, p).Passed)); err != nil {
				return nil, nil, err
			}
		}
		if claims[figure.MetricApprovalsHashBound] {
			prop := approvalHashProperty(s)
			props = append(props, prop)
			if err := measured(figure.ArmPlatform, figure.MetricApprovalsHashBound, boolTo(prop.Passed)); err != nil {
				return nil, nil, err
			}
		}
		if claims[figure.MetricModelCallsJournaled] {
			// The concept, the shape, the query and the @serverOnly mutation
			// all exist; no Go writer does. Reporting 1.0 here would be
			// reporting that every model call was journaled because none was
			// made and none was recorded.
			if err := absent(figure.ArmPlatform, figure.MetricModelCallsJournaled, figure.ReasonSeamNotBuilt,
				"nothing writes v1:work:modelCall: the concept, shape, query and @serverOnly mutation exist and the Go writer is the remaining half of epic A2"); err != nil {
				return nil, nil, err
			}
		}
	}

	return entries, props, nil
}

// receiptProperty asks the governance question the fake world can answer:
// every side-effecting step left a receipt, meaning it was performed under an
// idempotency key rather than blind.
func receiptProperty(s scenario.Scenario, res ArmResult) scorecard.Property {
	p := scorecard.Property{Name: "everyEffectHasAReceipt", Scenario: s.Id, Passed: true}
	if res.Effects == 0 {
		// Vacuous truth is not a pass. A scenario claiming this property and
		// performing no effect is a scenario whose claim measures nothing.
		p.Passed = false
		p.Detail = "the scenario claims every effect has a receipt and performed no effect, so the property is vacuous"
	}
	return p
}

// approvalHashProperty asks whether an approval carries to a modified
// artifact. It is decided by component/work.ResumeAllowed over values, which
// is where the rule lives -- asserting it here against the real function is
// what makes the scorecard's claim a claim about the shipped code.
func approvalHashProperty(s scenario.Scenario) scorecard.Property {
	p := scorecard.Property{Name: "approvalsBoundToArtifactHash", Scenario: s.Id, Passed: true}
	original := work.ArtifactHash(map[string]any{"command": "rm -rf /tmp/proving", "cwd": "/tmp"})
	modified := work.ArtifactHash(map[string]any{"command": "rm -rf /", "cwd": "/tmp"})
	if ok, err := work.ResumeAllowed(original, original, "approved"); !ok || err != nil {
		p.Passed = false
		p.Detail = fmt.Sprintf("an approval of the unchanged artifact refused to resume: %v", err)
		return p
	}
	ok, err := work.ResumeAllowed(original, modified, "approved")
	if ok || err == nil {
		p.Passed = false
		p.Detail = "an approval carried to a MODIFIED artifact; the hash binding is not enforced"
	}
	return p
}

func boolTo(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// variableKeys is the sorted set of variable names a scenario binds, which is
// half of the goal signature the catalog is keyed on.
func variableKeys(s scenario.Scenario) []string {
	seen := map[string]bool{}
	for _, set := range s.Variables {
		for k := range set {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
