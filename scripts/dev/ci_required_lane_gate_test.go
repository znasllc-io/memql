package dev

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Static guard over the set of lanes that actually GATE a merge
// (znasllc-io/memql#3019).
//
// # The defect
//
// Branch protection on this repo points at one aggregating check,
// `ci-required`, which reports the combined result of whatever is listed in
// its `needs:`. Removing a lane from that list does not make the lane
// disappear -- it still runs, still shows on the PR, still goes red on
// failure. It just stops **blocking the merge**. The PR page looks
// essentially identical, and nothing asserted on the list.
//
// Measured on PR #3015's branch: deleting `- vscode-extension` from
// `ci-required.needs` left every guard in vscode_lane_scope_test.go green and
// turned the lane advisory. It is a one-line diff that reads as a list tidy-up
// in review.
//
// # Why this is the last rung, and the widest
//
// Each previous rung of this ladder got its own guard, and each was scoped to
// one lane:
//
//	#2923 -- a `-run` pattern selecting no tests:   the TEST ran nothing.
//	#2792 -- a lane's package pattern missing them: the LANE tested the wrong thing.
//	#2792 review -- a job `if:` that stops matching: the LANE did not run.
//	#3019 -- this: the lane runs, is RED, and the merge proceeds anyway.
//
// This one is unguarded for every lane at once.
//
// # Derived, not pinned
//
// The issue offered two shapes: pin the known list, or derive it. Derived is
// implemented, because the pinned form misses the case that needs no edit to
// go wrong -- a NEW job added to ci.yml and never wired into `ci-required`.
// That job is advisory from the day it lands, and a pinned list stays green
// because nobody removed anything.
//
// The cost of deriving is that a deliberately-advisory lane needs an explicit
// opt-out. There are none today; `advisoryLanes` is empty and its emptiness is
// asserted to mean something.
//
// # What else is checked, and why it is not scope creep
//
// `ci-required` gating the right SET is necessary and not sufficient. Four
// more edits leave the set intact and re-open the same hole:
//
//	needs: [ ..., go-cheks ]      -- a typo names no job; the lane silently drops out
//	if: <anything but always()>   -- a failed lane SKIPS the aggregator instead of failing it
//	RESULTS: <not needs.*.result> -- the aggregator reports on a different set than it waits for
//	continue-on-error / || true   -- the aggregator goes red and the check reports green
//
// Each is the same defect wearing different punctuation, so they live under
// one guard rather than being discovered one at a time.
var advisoryLanes = map[string]string{
	// Deliberately NOT required to pass before a merge. Empty today.
	//
	// An entry here is a decision that a red lane may land, so it carries the
	// reason. TestAdvisoryLanesAreRealJobs keeps entries from outliving the
	// jobs they name -- a stale entry silently exempts nothing, or worse,
	// exempts a lane that was renamed into existence later.
}

// ciRequiredJob is the job branch protection actually requires.
const ciRequiredJob = "ci-required"

// laneExemptFromRequirement are the jobs that must NOT be required to appear in
// `needs` -- the aggregator itself, and the path-filter job every lane depends
// on (which IS in needs today, and legitimately, but must not be *demanded*
// there: it is infrastructure for the lanes rather than a lane).
var laneExemptFromRequirement = map[string]bool{
	ciRequiredJob: true,
	"changes":     true,
}

// ciJobs is the parsed job map, with the fields this guard reads.
type ciJobs struct {
	Jobs map[string]struct {
		If    string `yaml:"if"`
		Needs any    `yaml:"needs"`
		Steps []struct {
			Name            string            `yaml:"name"`
			Run             string            `yaml:"run"`
			If              string            `yaml:"if"`
			Env             map[string]string `yaml:"env"`
			ContinueOnError any               `yaml:"continue-on-error"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func parseCIJobs(t *testing.T) ciJobs {
	t.Helper()
	var doc ciJobs
	if err := yaml.Unmarshal(ciYAML(t), &doc); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	if len(doc.Jobs) == 0 {
		t.Fatal("parsed no jobs from .github/workflows/ci.yml -- this guard cannot pass vacuously")
	}
	return doc
}

// requiredLanes returns the `needs` list of the ci-required job.
//
// `needs` is a scalar OR a sequence in workflow syntax, so both are handled;
// a guard that only understood the list form would silently find nothing the
// day someone collapsed it to one entry.
func requiredLanes(t *testing.T, doc ciJobs) []string {
	t.Helper()
	job, ok := doc.Jobs[ciRequiredJob]
	if !ok {
		t.Fatalf("no %q job in .github/workflows/ci.yml. Branch protection requires that check "+
			"by name, so if the aggregator was renamed the ruleset must be updated with it -- and "+
			"this guard retargeted rather than deleted (memql#3019)", ciRequiredJob)
	}
	var out []string
	switch n := job.Needs.(type) {
	case string:
		out = []string{n}
	case []any:
		for _, v := range n {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%q declares no `needs`. It would report success having waited for nothing, so "+
			"EVERY lane becomes advisory at once -- the widest form of memql#3019", ciRequiredJob)
	}
	return out
}

// Every lane must gate the merge unless it is explicitly advisory.
//
// This is the assertion that fails against the measured defect: delete
// `- vscode-extension` from `ci-required.needs` and this reds, where every
// other guard in this package stays green.
func TestEveryCILaneGatesTheMerge(t *testing.T) {
	doc := parseCIJobs(t)
	required := map[string]bool{}
	for _, n := range requiredLanes(t, doc) {
		required[n] = true
	}

	var lanes int
	for name := range doc.Jobs {
		if laneExemptFromRequirement[name] {
			continue
		}
		lanes++
		if required[name] {
			if reason, advisory := advisoryLanes[name]; advisory {
				t.Errorf("%q is listed in advisoryLanes (%q) but IS required. Remove the entry -- "+
					"an advisory record that no longer describes the lane is how the next reader "+
					"comes to believe a gating lane is optional.", name, reason)
			}
			continue
		}
		if _, advisory := advisoryLanes[name]; advisory {
			continue
		}
		t.Errorf("job %q is not in %s's `needs`, so it does NOT gate a merge.\n"+
			"The lane still runs, still shows on the PR, and still goes red on failure -- it "+
			"simply stops blocking the merge, which is why this is invisible in review "+
			"(memql#3019). Add it to `needs`, or add it to advisoryLanes with the reason a red "+
			"result there may land.", name, ciRequiredJob)
	}

	// A workflow that stopped parsing into jobs, or a rename of every lane,
	// would leave the loop with nothing to check and report clean.
	if lanes == 0 {
		t.Fatal("found no non-exempt jobs in ci.yml -- either every lane was renamed or the parse " +
			"narrowed, and this guard would now pass vacuously (memql#3019)")
	}

	// The converse: a `needs` entry naming no job. GitHub errors on this at
	// workflow level, but a guard that only checked one direction would report
	// a typo'd lane as covered right up until the workflow was next edited.
	for _, n := range requiredLanes(t, doc) {
		if _, ok := doc.Jobs[n]; !ok {
			t.Errorf("%s `needs` names %q, which is not a job in ci.yml. The lane it was meant to "+
				"name is not gating anything.", ciRequiredJob, n)
		}
	}
}

// Gating the right SET is necessary and not sufficient: four more edits leave
// the set intact and re-open the hole. Each is checked here rather than
// discovered one at a time.
func TestCIRequiredAggregatorCannotFailOpen(t *testing.T) {
	doc := parseCIJobs(t)
	job := doc.Jobs[ciRequiredJob]

	// 1. `if: always()`. Without it a FAILED lane skips the aggregator rather
	//    than failing it -- and a skipped required check does not block, so the
	//    one edit that matters most reads as a cleanup.
	if !strings.Contains(strings.ReplaceAll(job.If, " ", ""), "always()") {
		t.Errorf("%s is not gated on `always()` (got if: %q). Without it the aggregator SKIPS "+
			"when a lane fails instead of reporting the failure, and a skipped required check "+
			"does not block a merge -- the same fail-open as dropping the lane (memql#3019).",
			ciRequiredJob, job.If)
	}

	if len(job.Steps) == 0 {
		t.Fatalf("%s declares no steps, so it reports success having verified nothing", ciRequiredJob)
	}

	var sawResults, sawExit bool
	for _, s := range job.Steps {
		// 2. The aggregator must read its results from the same `needs` it
		//    waits on. A hardcoded or narrowed expression would report on a
		//    different set than the guard above checks -- the guard would be
		//    green and the gate still open.
		for k, v := range s.Env {
			if k != "RESULTS" {
				continue
			}
			sawResults = true
			if !strings.Contains(strings.ReplaceAll(v, " ", ""), "needs.*.result") {
				t.Errorf("%s's RESULTS is %q, which does not derive from `needs.*.result`. The "+
					"aggregator would then report on a different set of lanes than it depends "+
					"on, and TestEveryCILaneGatesTheMerge would be asserting over the wrong "+
					"list (memql#3019).", ciRequiredJob, v)
			}
		}

		// 3. A step whose failure is swallowed is not a gate. Read from the
		//    PARSED step, never the command text -- continue-on-error is a step
		//    key and can never appear in a command, a mistake an adversarial
		//    review of the sibling guard already caught once.
		if s.ContinueOnError != nil && s.ContinueOnError != false {
			t.Errorf("%s step %q sets continue-on-error=%v; the aggregator would report success "+
				"whatever the lanes did", ciRequiredJob, s.Name, s.ContinueOnError)
		}
		if strings.TrimSpace(s.If) != "" {
			t.Errorf("%s step %q is gated on `if: %s`. The verification step must run whenever "+
				"the job does; a condition that evaluates false reports green having checked "+
				"nothing.", ciRequiredJob, s.Name, s.If)
		}

		pipefail := false
		for _, line := range commandLines(s.Run) {
			if strings.Contains(line, "pipefail") {
				pipefail = true
			}
		}
		for _, line := range commandLines(s.Run) {
			for _, esc := range []string{"|| true", "|| :", "|| exit 0"} {
				if strings.Contains(line, esc) {
					t.Errorf("%s suppresses its failure exit (%q).\ngot: %s", ciRequiredJob, esc, line)
				}
			}
			if !pipefail && hasPipe(line) && strings.Contains(line, "exit") {
				t.Errorf("%s pipes a command that decides the exit status without `pipefail`; "+
					"the step would take the LAST command's status.\ngot: %s", ciRequiredJob, line)
			}
			// 4. Something has to actually fail the job. A verification script
			//    that only echoes is the purest form of this defect.
			if strings.Contains(line, "exit 1") {
				sawExit = true
			}
		}
	}

	if !sawResults {
		t.Errorf("%s declares no RESULTS env. The verification step reads the lane results from "+
			"it, so without it the aggregator checks an empty string and passes unconditionally "+
			"(memql#3019).", ciRequiredJob)
	}
	if !sawExit {
		t.Errorf("%s never exits non-zero. A required check that cannot fail is not a gate -- "+
			"every lane is advisory and the PR page looks exactly the same (memql#3019).",
			ciRequiredJob)
	}
}

// An advisory entry naming a job that does not exist exempts nothing, and
// would keep exempting nothing forever. Empty today, and its emptiness is
// asserted to MEAN something rather than being incidental.
func TestAdvisoryLanesAreRealJobs(t *testing.T) {
	doc := parseCIJobs(t)
	for name, reason := range advisoryLanes {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("advisoryLanes[%q] carries no reason. The reason is the whole mechanism: an "+
				"entry here says a RED lane may land, which is a decision someone has to own.", name)
		}
		if _, ok := doc.Jobs[name]; !ok {
			t.Errorf("advisoryLanes names %q, which is not a job in ci.yml. Retarget it at "+
				"whatever replaced the lane, or remove it.", name)
		}
	}
	if len(advisoryLanes) != 0 {
		t.Logf("NOTE: %d lane(s) are deliberately advisory and do not gate a merge: %v",
			len(advisoryLanes), advisoryLanes)
	}
}
