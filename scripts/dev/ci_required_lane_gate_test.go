package dev

import (
	"os"
	"os/exec"
	"path/filepath"
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
// What this file does NOT cover, said plainly so the paragraph above is not
// read as more than it is: a lane whose own job-level `if:` stops matching is
// still guarded only per-lane, and only for three of the eight -- go-checks
// (gate_inputs_lane_scope_test.go), vscode-extension (vscode_lane_scope_test.go)
// and db-tests (scripts/cidb). Setting `if: ${{ false }}` on conformance or
// build-node-tags leaves this file green, because the lane is still LISTED in
// `needs` and a skipped lane counts as a pass. That rung belongs to #2792 and
// is not closed for every lane; the set-membership rung is what closes here.
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
		If              string            `yaml:"if"`
		Needs           any               `yaml:"needs"`
		ContinueOnError any               `yaml:"continue-on-error"`
		Env             map[string]string `yaml:"env"`
		Steps           []struct {
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

	// 1. `if:` must be always() and NOTHING ELSE. Without always() a FAILED
	//    lane skips the aggregator rather than failing it, and a skipped
	//    required check does not block -- so the one edit that matters most
	//    reads as a cleanup.
	//
	//    Substring-matching "always()" is not enough, and this was measured
	//    rather than reasoned: `if: ${{ always() && github.event_name ==
	//    'push' }}` CONTAINS always() and still skips the whole aggregator on
	//    a pull_request. That is the identical fail-open wearing the token
	//    that is supposed to prevent it. Compare the normalised whole
	//    expression instead.
	if normalizedIf := normalizeExpr(job.If); normalizedIf != "always()" {
		t.Errorf("%s must be gated on exactly `always()`; got if: %q (normalised %q).\n"+
			"Without always() the aggregator SKIPS when a lane fails instead of reporting the "+
			"failure. And a NARROWED always() -- `always() && <cond>` -- is no better: when "+
			"<cond> is false the job skips, and GitHub treats a skipped required check as "+
			"satisfied, so the merge proceeds with red lanes (memql#3019).",
			ciRequiredJob, job.If, normalizedIf)
	}

	if len(job.Steps) == 0 {
		t.Fatalf("%s declares no steps, so it reports success having verified nothing", ciRequiredJob)
	}

	// The aggregator may carry RESULTS at job level or step level -- the two
	// are semantically identical in Actions, so demanding the step form turned
	// a legitimate refactor into a failure whose message misdescribed the
	// state (it claimed RESULTS was absent when it had merely moved).
	var sawResults bool
	checkResults := func(where, k, v string) {
		if k != "RESULTS" {
			return
		}
		sawResults = true
		if !strings.Contains(strings.ReplaceAll(v, " ", ""), "needs.*.result") {
			t.Errorf("%s's RESULTS (%s) is %q, which does not derive from `needs.*.result`. The "+
				"aggregator would then report on a different set of lanes than it depends on, "+
				"and TestEveryCILaneGatesTheMerge would be asserting over the wrong list "+
				"(memql#3019).", ciRequiredJob, where, v)
		}
	}
	for k, v := range job.Env {
		checkResults("job env", k, v)
	}

	for _, s := range job.Steps {
		// 2. The aggregator must read its results from the same `needs` it
		//    waits on. A hardcoded or narrowed expression would report on a
		//    different set than the guard above checks -- the guard would be
		//    green and the gate still open.
		for k, v := range s.Env {
			checkResults("step env", k, v)
		}

		// 3. A step whose failure is swallowed is not a gate. Read from the
		//    PARSED step, never the command text -- continue-on-error is a step
		//    key and can never appear in a command, a mistake an adversarial
		//    review of the sibling guard already caught once.
		if s.ContinueOnError != nil && s.ContinueOnError != false {
			t.Errorf("%s step %q sets continue-on-error=%v; the aggregator would report success "+
				"whatever the lanes did", ciRequiredJob, s.Name, s.ContinueOnError)
		}
		// Only the VERIFYING step must be unconditional -- the one that reads
		// RESULTS and decides the exit status. Applying this to every step
		// blocked legitimate additions (a `if: ${{ failure() }}` diagnostic
		// dump) for no gate-related reason, and both sibling guards scope it
		// the same way: vscode_lane_scope_test.go narrows to goTestSteps and
		// gate_inputs_lane_scope_test.go to gateInputsStep.
		if _, verifies := s.Env["RESULTS"]; (verifies || strings.Contains(s.Run, "RESULTS")) &&
			strings.TrimSpace(s.If) != "" {
			t.Errorf("%s's verification step %q is gated on `if: %s`. The step that decides the "+
				"job's exit status must run whenever the job does; a condition that evaluates "+
				"false reports green having checked nothing (memql#3019).",
				ciRequiredJob, s.Name, s.If)
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
		}
	}

	if !sawResults {
		t.Errorf("%s declares no RESULTS env. The verification step reads the lane results from "+
			"it, so without it the aggregator checks an empty string and passes unconditionally "+
			"(memql#3019).", ciRequiredJob)
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

// normalizeExpr strips `${{ }}` wrapping and all whitespace so a workflow
// expression can be compared whole rather than by substring. Substring
// matching is what let `always() && <cond>` pass for the token it contains.
func normalizeExpr(raw string) string {
	e := strings.TrimSpace(raw)
	e = strings.TrimPrefix(e, "${{")
	e = strings.TrimSuffix(e, "}}")
	return strings.Join(strings.Fields(e), "")
}

// The aggregator must actually exit non-zero on a red lane. RUN IT.
//
// This replaces a `strings.Contains(line, "exit 1")` check, and the reason is
// worth keeping: that assertion was a proxy for the property, and three
// separate one-line edits satisfied the proxy while destroying the property.
//
//	drop `status=1` from the failure arm -- `exit 1` is still present in the
//	  text, and now unreachable. The step echoes FAIL and exits 0.
//	put a decoy `exit 1` in any other step of the job -- the check was
//	  job-wide, so the real verification step could be gutted entirely.
//	change the loop to `for r in $OTHER` -- RESULTS is still declared, still
//	  derives from needs.*.result, and is never read.
//
// Each of those is exactly memql#3019: the lane is red and the merge proceeds.
// None is visible to a text search, and all three are caught by running the
// script and looking at the exit status, which is the property itself rather
// than a spelling that usually accompanies it.
//
// The script is pure bash over $RESULTS with no Actions context, which is what
// makes this possible at all -- if it ever grows a `${{ }}` interpolation
// inside `run:`, this test must switch to rendering it first rather than being
// deleted.
func TestCIRequiredAggregatorActuallyFailsOnARedLane(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		// Not a silent skip: every platform this repo builds on has bash, so
		// its absence means the environment is wrong, not that the property
		// stopped mattering.
		t.Fatalf("bash not found (%v) -- this guard executes the aggregator script, and skipping "+
			"it would silently retire the only check that the gate can actually fail", err)
	}

	doc := parseCIJobs(t)
	job := doc.Jobs[ciRequiredJob]

	var script string
	for _, s := range job.Steps {
		if _, ok := s.Env["RESULTS"]; ok || strings.Contains(s.Run, "RESULTS") {
			script = s.Run
			break
		}
	}
	if strings.TrimSpace(script) == "" {
		t.Fatalf("found no step in %s that reads RESULTS, so there is nothing that decides the "+
			"job's exit status (memql#3019)", ciRequiredJob)
	}
	if strings.Contains(script, "${{") {
		t.Fatalf("%s's verification script interpolates a `${{ }}` expression, which this test "+
			"cannot evaluate. Render it before running, or this check silently stops proving "+
			"the aggregator can fail:\n%s", ciRequiredJob, script)
	}

	path := filepath.Join(t.TempDir(), "verify.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	for _, tc := range []struct {
		results  string
		wantFail bool
		why      string
	}{
		{"success success success", false, "every lane green must pass"},
		{"success skipped success", false, "a path-skipped lane is a deliberate pass here"},
		{"skipped skipped", false, "an all-skipped run (no relevant paths touched) passes"},
		{"success failure", true, "A FAILED LANE MUST FAIL THE AGGREGATOR -- this is memql#3019"},
		{"failure", true, "a single failed lane must fail it"},
		{"success cancelled", true, "a cancelled lane is not a pass"},
		{"success timed_out", true, "a timed-out lane is not a pass"},
		{"", true, "an EMPTY result set must not report success: it means the aggregator waited " +
			"on nothing, which is every lane advisory at once"},
	} {
		t.Run(strings.ReplaceAll(tc.results, " ", "_"), func(t *testing.T) {
			cmd := exec.Command(bash, path)
			cmd.Env = append(os.Environ(), "RESULTS="+tc.results)
			out, runErr := cmd.CombinedOutput()
			failed := runErr != nil
			if failed != tc.wantFail {
				t.Errorf("RESULTS=%q: aggregator failed=%v, want %v.\n%s\noutput:\n%s",
					tc.results, failed, tc.wantFail, tc.why, out)
			}
		})
	}
}

// No LANE may swallow its own failure.
//
// The set check proves a lane is listed in `needs`. It does not prove the lane
// can report a failure: `continue-on-error: true` at JOB level makes a red lane
// report success into `needs.*.result`, so the aggregator sees green and the
// merge proceeds. The lane still runs and still shows red on the PR page --
// the memql#3019 sentence exactly, reached by a one-line diff on a different
// line than the needs list.
//
// The sibling guard in scripts/cidb already treats this as a member of the
// family, but only for db-tests. Nothing covered the other seven lanes or the
// aggregator itself.
func TestNoLaneSwallowsItsOwnFailure(t *testing.T) {
	doc := parseCIJobs(t)
	var checked int
	for name, job := range doc.Jobs {
		checked++
		if job.ContinueOnError == nil || job.ContinueOnError == false {
			continue
		}
		if reason, advisory := advisoryLanes[name]; advisory {
			t.Logf("%q is continue-on-error and is a declared advisory lane (%s)", name, reason)
			continue
		}
		t.Errorf("job %q sets continue-on-error=%v. It reports SUCCESS into `needs.*.result` "+
			"however it actually finished, so %s sees green and the merge proceeds while the "+
			"lane shows red on the PR (memql#3019). Either drop it, or declare the lane in "+
			"advisoryLanes with the reason a red result there may land.",
			name, job.ContinueOnError, ciRequiredJob)
	}
	if checked == 0 {
		t.Fatal("checked no jobs -- this guard would pass vacuously")
	}
}

// laneExemptFromRequirement must name jobs that exist.
//
// advisoryLanes already has this guard; this map did not, and the asymmetry was
// exploitable rather than cosmetic. Measured: rename `changes` to
// `paths-filter` and the suite stays green with the exemption naming nothing.
// Then add a NEW always-failing job called `changes` and leave it out of
// `needs` -- it is silently exempted by the now-stale key, which is a red,
// non-gating lane, i.e. the whole point of memql#3019.
func TestLaneExemptionsNameRealJobs(t *testing.T) {
	doc := parseCIJobs(t)
	for name := range laneExemptFromRequirement {
		if _, ok := doc.Jobs[name]; !ok {
			t.Errorf("laneExemptFromRequirement names %q, which is not a job in ci.yml. The "+
				"entry now exempts nothing -- and worse, it silently exempts any FUTURE job "+
				"that takes the name, without that job ever having to gate a merge. Retarget "+
				"it at whatever replaced the job, or remove it.", name)
		}
	}

	// `changes` is exempt from being DEMANDED in needs because it is
	// infrastructure rather than a lane. It must still BE there: every lane
	// declares `needs: changes`, so dropping it leaves them all `skipped`,
	// which this aggregator counts as a pass. That is the shape recorded on
	// PR #2854 in gate_inputs_lane_scope_test.go.
	required := map[string]bool{}
	for _, n := range requiredLanes(t, doc) {
		required[n] = true
	}
	if !required["changes"] {
		t.Errorf("%s does not depend on `changes`. Every lane is `needs: changes`, so if the "+
			"path filter does not run they all report `skipped` -- which the aggregator treats "+
			"as a pass. The result is a green required check over lanes that never ran.",
			ciRequiredJob)
	}
}
