package proving

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/scorecard"
)

// SuiteResult is one whole run of the corpus.
type SuiteResult struct {
	Scorecard scorecard.Scorecard
	// Results are every arm result, for the row writer and for a failure that
	// needs to say what happened.
	Results []ArmResult
	// ControlFailures are negative controls that read the wrong way -- a
	// lower-is-better control that came back zero, or a higher-is-better one
	// that did not. THEY ARE FATAL, and they are kept separate from ordinary
	// verifier failures because they mean something different: not "the
	// platform regressed" but "the instrument is dead, and every green figure
	// it produced means nothing".
	ControlFailures []string
	// VerifierFailures are scenarios whose platform arm did not satisfy its
	// own verifier.
	VerifierFailures []string
}

// Run executes the whole corpus on both arms and assembles the scorecard.
func (r *Runner) Run(ctx context.Context, corpus scenario.Corpus) (SuiteResult, error) {
	if err := corpus.CorpusControls(); err != nil {
		return SuiteResult{}, err
	}

	out := SuiteResult{
		Scorecard: scorecard.Scorecard{
			Version:           scorecard.CurrentVersion,
			Date:              r.Prov.Date,
			Commit:            r.Prov.Commit,
			CorpusFingerprint: corpus.Fingerprint,
			Tiers:             map[figure.Tier]scorecard.TierState{},
		},
	}

	for _, s := range corpus.Scenarios {
		results := map[figure.Arm]ArmResult{}
		for _, arm := range []figure.Arm{figure.ArmPlatform, figure.ArmBaseline} {
			res := r.RunScenario(ctx, s, arm)
			if res.Err != nil {
				return SuiteResult{}, fmt.Errorf("%s/%s: %w", s.Id, arm, res.Err)
			}
			results[arm] = res
			out.Results = append(out.Results, res)
		}

		if p := results[figure.ArmPlatform]; !p.Passed {
			out.VerifierFailures = append(out.VerifierFailures,
				fmt.Sprintf("%s: %s", s.Id, strings.Join(p.Failures, "; ")))
		}

		// The journal-overhead measurement is its own instrument -- the same
		// automation run twice in one process, journal on and journal off --
		// so it runs only for the scenario that claims it and costs nothing
		// for the other seventeen.
		var overhead *Overhead
		for _, m := range s.Claims {
			if m != figure.MetricJournalOverhead {
				continue
			}
			ratio, ok, reason := r.MeasureJournalOverhead(ctx, s)
			overhead = &Overhead{Ratio: ratio, OK: ok, Reason: reason}
			break
		}

		entries, props, err := Figures(s, results, overhead, r.Prov)
		if err != nil {
			return SuiteResult{}, err
		}
		out.Scorecard.Entries = append(out.Scorecard.Entries, entries...)
		out.Scorecard.Governance = append(out.Scorecard.Governance, props...)

		if msg := checkNegativeControl(s, entries); msg != "" {
			out.ControlFailures = append(out.ControlFailures, msg)
		}
	}

	out.Scorecard.Sort()
	sort.Strings(out.ControlFailures)
	sort.Strings(out.VerifierFailures)
	return out, nil
}

// checkNegativeControl is the instrument check, and it is the most important
// assertion in the suite.
//
// A counter that is never incremented on ANY path reads as zero forever. Every
// blocking metric whose good answer is zero -- duplicated side effects, steps
// re-executed, provider calls on a replay -- is therefore paired with a
// scenario that MUST produce a non-zero, and this is where "must" is enforced.
//
// THE RULE HAS TWO DIRECTIONS, because a counter can be dead in two ways
// (figure.Spec.Control). One whose good answer is a POSITIVE count -- goals a
// trusted procedure served with no model -- is dead when it reads its claim on
// every path: "no model was reached" is also true of a goal nothing served at
// all. Its control is a scenario in which the counted event does not happen,
// and it MUST read ZERO.
//
// A lower-is-better control is normally the BASELINE ARM of the same
// scenario: a bare loop with no journal restarts from the beginning, so it
// re-executes and it re-delivers.
//
// TWO LOWER-IS-BETTER METRICS ARE MEASURED ON THE PLATFORM ARM INSTEAD, and
// the reason is the same both times: the mechanism the metric counts does not
// exist in a bare loop at all, so its baseline arm is structurally zero and
// would report a dead instrument on a working one. `compileCallsOnCatalogHit`
// -- a bare loop does not compile. `recovery.modelCalls` -- a bare loop has no
// failure path, so it never reaches the symptom classifier; the count the
// metric is about is the PLATFORM deciding whether a failure needs a model,
// and a control on the baseline would be asking a question the arm cannot
// answer.
//
// That is a narrower exemption than it looks. On the platform arm the control
// still has to produce a non-zero, which is the whole property: it fails if the
// classifier is never reached, which is exactly the state the suite was in
// before it was wired.
//
// EVERY HIGHER-IS-BETTER CONTROL IS ON THE PLATFORM ARM, for the mirror
// reason: the baseline's reading is structurally zero -- a bare loop has no
// ladder, so it serves no goal from a procedure -- and a zero there proves
// nothing about the counter. The lie a positive counter tells is the
// PLATFORM claiming an event that did not happen, so that is where its control
// must read zero.
//
// platformArmControls are the lower-is-better metrics whose negative control is
// measured on the platform arm rather than the baseline. See the header above
// for why each is here; an entry added without that reasoning turns a control
// into a formality.
var platformArmControls = map[figure.Metric]bool{
	figure.MetricCompileCallsExact: true,
	figure.MetricRecoveryCalls:     true,
}

// controlArm is the arm a metric's negative control is read on.
func controlArm(m figure.Metric) figure.Arm {
	spec, _ := figure.MetricSpec(m)
	if platformArmControls[m] || spec.Control() == figure.ControlZero {
		return figure.ArmPlatform
	}
	return figure.ArmBaseline
}

func checkNegativeControl(s scenario.Scenario, entries []scorecard.Entry) string {
	m := s.NegativeControlFor
	if m == "" {
		return ""
	}
	spec, ok := figure.MetricSpec(m)
	if !ok {
		return fmt.Sprintf("%s is the negative control for %s, which is not a registered metric", s.Id, m)
	}
	arm := controlArm(m)
	for _, e := range entries {
		if e.Figure.Metric != m || e.Arm != arm {
			continue
		}
		if !e.Figure.IsMeasured() {
			return fmt.Sprintf("%s is the negative control for %s and its %s figure is unmeasured (%s), so nothing checks the instrument",
				s.Id, m, arm, e.Figure.Absent)
		}
		switch spec.Control() {
		case figure.ControlNonZero:
			if e.Figure.Stat.Median == 0 {
				return fmt.Sprintf(
					"%s is the negative control for %s and its %s arm measured ZERO. "+
						"That means the counter behind %s never goes up on any path, so every green figure it produced means nothing. "+
						"Fix the instrument before believing the suite",
					s.Id, m, arm, m)
			}
		case figure.ControlZero:
			if e.Figure.Stat.Median != 0 {
				return fmt.Sprintf(
					"%s is the negative control for %s and its %s arm measured %s where the event it counts does not happen. "+
						"That means the counter behind %s reads its claim whether or not the event occurred, so every green figure it produced means nothing. "+
						"Fix the instrument before believing the suite",
					s.Id, m, arm, e.Figure.Render(), m)
			}
		default:
			return fmt.Sprintf("%s is the negative control for %s, which declares no better direction, so no reading of it means anything", s.Id, m)
		}
		return ""
	}
	return fmt.Sprintf("%s is the negative control for %s but produced no %s figure on the %s arm", s.Id, m, m, arm)
}

// Blocking reports whether the suite's own run failed, before any comparison
// against a previous scorecard. A verifier failure and a dead instrument both
// block; a cost movement does not (design P2).
func (r SuiteResult) Blocking() []string {
	var out []string
	out = append(out, r.ControlFailures...)
	out = append(out, r.VerifierFailures...)
	for _, p := range r.Scorecard.Governance {
		if !p.Passed {
			out = append(out, fmt.Sprintf("governance %s failed on %s: %s", p.Name, p.Scenario, p.Detail))
		}
	}
	return out
}
