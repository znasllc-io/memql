package scorecard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/proving/figure"
)

// GateResult is the regression gate's answer.
//
// Design P2: STRUCTURAL PROPERTIES BLOCK, COST AND SPEED REPORT. A scenario
// that stops passing, a replay that reaches a provider, a duplicated side
// effect or a failed governance property fails the pull request. Cost and
// speed deltas are computed and published and never red.
//
// The reason for the split is not squeamishness. A cost threshold on a shared
// CI runner reds the lane for runner noise; the first fix anybody reaches for
// is widening the threshold until it means nothing; and the structural half
// dies with it, because a lane people have learned to ignore protects nothing.
type GateResult struct {
	// Blocking are the regressions that fail the build.
	Blocking []Finding `json:"blocking"`
	// Reported are the cost and speed movements, published either way.
	Reported []Finding `json:"reported"`
	// Improvements are published beside the regressions. The epic's honesty
	// rule names this explicitly: regressions are published WITH improvements,
	// which stops the gate's output reading as a list of bad news nobody
	// believes.
	Improvements []Finding `json:"improvements"`
	// Undecidable are the comparisons that could not be made, with the reason.
	// They are reported rather than dropped: a comparison silently skipped is
	// indistinguishable from one that passed.
	Undecidable []Finding `json:"undecidable"`
	// NewMetrics are figures with no counterpart in the baseline scorecard.
	// Not a regression, and not nothing: a metric appearing is worth a line.
	NewMetrics []string `json:"newMetrics"`
	// LostMetrics are figures the baseline had and this run does not. THIS IS
	// A BLOCKING CONDITION, and it is the one a naive gate misses: a suite
	// that stops measuring something reports no regression forever.
	LostMetrics []string `json:"lostMetrics"`
}

// Finding is one comparison worth printing.
type Finding struct {
	Scenario string         `json:"scenario"`
	Arm      figure.Arm     `json:"arm"`
	Metric   figure.Metric  `json:"metric"`
	Verdict  figure.Verdict `json:"verdict"`
	Detail   string         `json:"detail"`
}

// Passed reports whether the gate lets the merge through.
func (g GateResult) Passed() bool { return len(g.Blocking) == 0 && len(g.LostMetrics) == 0 }

// Gate compares a fresh scorecard against the committed one.
//
// A nil-valued `before` (no committed scorecard yet) is not a failure: the
// first run has nothing to regress against. It is reported as such rather than
// passing silently, because "the gate found nothing to compare" and "the gate
// found no regressions" are different states and only one of them is
// reassuring.
func Gate(before, now Scorecard, haveBefore bool) GateResult {
	var g GateResult

	// Governance is pass-or-fail and needs no comparison: a property that
	// failed in THIS run blocks, whether or not it failed in the last one.
	for _, p := range now.Governance {
		if p.Passed {
			continue
		}
		g.Blocking = append(g.Blocking, Finding{
			Scenario: p.Scenario,
			Metric:   figure.Metric("governance." + p.Name),
			Verdict:  figure.Regressed,
			Detail:   "governance property failed: " + p.Detail,
		})
	}

	if !haveBefore {
		g.NewMetrics = append(g.NewMetrics, "(no committed scorecard: nothing to compare against)")
		return g
	}

	type key struct {
		scenario string
		arm      figure.Arm
		metric   figure.Metric
	}
	prev := map[key]figure.Figure{}
	for _, e := range before.Entries {
		prev[key{e.Scenario, e.Arm, e.Figure.Metric}] = e.Figure
	}
	cur := map[key]figure.Figure{}
	for _, e := range now.Entries {
		cur[key{e.Scenario, e.Arm, e.Figure.Metric}] = e.Figure
	}

	for _, e := range now.Entries {
		k := key{e.Scenario, e.Arm, e.Figure.Metric}
		p, ok := prev[k]
		if !ok {
			g.NewMetrics = append(g.NewMetrics, fmt.Sprintf("%s/%s/%s", e.Scenario, e.Arm, e.Figure.Metric))
			continue
		}
		d := figure.Compare(p, e.Figure)
		f := Finding{Scenario: e.Scenario, Arm: e.Arm, Metric: e.Figure.Metric, Verdict: d.Verdict, Detail: d.Render()}
		switch d.Verdict {
		case figure.Regressed:
			if d.Blocking {
				g.Blocking = append(g.Blocking, f)
			} else {
				g.Reported = append(g.Reported, f)
			}
		case figure.Improved:
			g.Improvements = append(g.Improvements, f)
		case figure.Undecidable:
			g.Undecidable = append(g.Undecidable, f)
		}
	}

	// The check a naive gate skips: a metric the previous scorecard carried
	// and this one does not. A suite that quietly stops measuring something
	// reports no regression forever, which is the most comfortable possible
	// way to be broken.
	for k := range prev {
		if _, ok := cur[k]; !ok {
			g.LostMetrics = append(g.LostMetrics, fmt.Sprintf("%s/%s/%s", k.scenario, k.arm, k.metric))
		}
	}
	sort.Strings(g.LostMetrics)
	sort.Strings(g.NewMetrics)
	sortFindings(g.Blocking)
	sortFindings(g.Reported)
	sortFindings(g.Improvements)
	sortFindings(g.Undecidable)
	return g
}

func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Metric != f[j].Metric {
			return f[i].Metric < f[j].Metric
		}
		if f[i].Scenario != f[j].Scenario {
			return f[i].Scenario < f[j].Scenario
		}
		return f[i].Arm < f[j].Arm
	})
}

// Render turns the gate's answer into the text CI prints. Improvements come
// FIRST when there are no blocking findings and LAST when there are, because a
// reader looking at a red lane wants the reason on the first screen.
func (g GateResult) Render() string {
	var b strings.Builder
	if g.Passed() {
		b.WriteString("PASS: no blocking regression.\n")
	} else {
		b.WriteString("FAIL: the proving gate blocked this change.\n")
	}

	writeSection := func(title string, items []Finding) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (%d)\n", title, len(items))
		for _, f := range items {
			fmt.Fprintf(&b, "  - %s / %s / %s: %s\n", f.Scenario, f.Arm, f.Metric, f.Detail)
		}
	}

	writeSection("BLOCKING", g.Blocking)
	if len(g.LostMetrics) > 0 {
		fmt.Fprintf(&b, "\nBLOCKING -- metrics that stopped being measured (%d)\n", len(g.LostMetrics))
		for _, m := range g.LostMetrics {
			fmt.Fprintf(&b, "  - %s\n", m)
		}
		b.WriteString("  A suite that stops measuring something reports no regression forever.\n" +
			"  If a scenario was deliberately removed, remove its entries from the committed\n" +
			"  scorecard in the same change so the two agree.\n")
	}
	writeSection("REPORTED -- cost and speed, which never block", g.Reported)
	writeSection("IMPROVED", g.Improvements)
	writeSection("UNDECIDABLE -- reported rather than dropped, so a skipped comparison is visible", g.Undecidable)
	if len(g.NewMetrics) > 0 {
		fmt.Fprintf(&b, "\nNEW (%d)\n", len(g.NewMetrics))
		for _, m := range g.NewMetrics {
			fmt.Fprintf(&b, "  - %s\n", m)
		}
	}
	return b.String()
}
