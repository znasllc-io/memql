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
	// ControlFailures are negative controls that came back zero. THEY ARE
	// FATAL, and they are kept separate from ordinary verifier failures
	// because they mean something different: not "the platform regressed" but
	// "the instrument is dead, and every green figure it produced means
	// nothing".
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

		entries, props, err := Figures(s, results, r.Prov)
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
// The control is normally the BASELINE ARM of the same scenario: a bare loop
// with no journal restarts from the beginning, so it re-executes and it
// re-delivers. `compileCallsOnCatalogHit` is the exception -- a bare loop does
// not compile -- and there the control is the platform arm measured on the
// catalog-MISS path.
func checkNegativeControl(s scenario.Scenario, entries []scorecard.Entry) string {
	m := s.NegativeControlFor
	if m == "" {
		return ""
	}
	arm := figure.ArmBaseline
	if m == figure.MetricCompileCallsExact {
		arm = figure.ArmPlatform
	}
	for _, e := range entries {
		if e.Figure.Metric != m || e.Arm != arm {
			continue
		}
		if !e.Figure.IsMeasured() {
			return fmt.Sprintf("%s is the negative control for %s and its %s figure is unmeasured (%s), so nothing checks the instrument",
				s.Id, m, arm, e.Figure.Absent)
		}
		if e.Figure.Stat.Median == 0 {
			return fmt.Sprintf(
				"%s is the negative control for %s and its %s arm measured ZERO. "+
					"That means the counter behind %s never goes up on any path, so every green figure it produced means nothing. "+
					"Fix the instrument before believing the suite",
				s.Id, m, arm, m)
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
