package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ruleset_drift_test.go -- memql#3836.
//
// The script under test asserts that `main`'s protection ruleset still carries
// the rules this repository expects. These tests drive the REAL script against
// a fake `gh`, so what is measured is what the script does with an answer --
// not what a reimplementation of its logic would do.
//
// THE THREE OUTCOMES THAT MATTER, and the reason each has a test rather than
// only the happy path:
//
//   - an expected rule MISSING must FAIL. That is the 2026-08-06 event.
//   - a known-absent rule REAPPEARING must FAIL. A baseline that keeps
//     excusing an absence after it is fixed is a baseline slowly becoming
//     fiction, and nothing else would ever notice.
//   - the known-absent rule staying absent must PASS **and warn**, every run.
//     Failing would make the check permanently red and therefore muted, which
//     is how the gap survives a second time; passing silently would make it
//     green over the exact defect it was built for.
//
// And a fourth that is not about drift at all: an unreadable ruleset must be
// distinguishable from a clean one. "I could not ask" reported as "no drift" is
// the failure this whole class of check keeps producing.

// fakeGH answers the one `gh api` call the script makes. The script asks for
// two facts in one read, so the fake answers in the same shape: line 1 is the
// enforcement state (FAKE_ENFORCEMENT, default active), line 2 the
// space-separated rule list (FAKE_RULES). FAKE_GH_FAILS makes the call fail,
// which is how the unreadable case is exercised.
const fakeGH = `#!/usr/bin/env bash
if [[ -n "${FAKE_GH_FAILS:-}" ]]; then
  echo "HTTP 404: Not Found" >&2
  exit 1
fi
printf '%s\n%s' "${FAKE_ENFORCEMENT:-active}" "$FAKE_RULES"
`

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// runDrift executes the real scripts/ci/ruleset-drift.sh with a fake gh
// reporting `rules`. Returns combined stderr and the exit code.
func runDrift(t *testing.T, rules string, ghFails bool) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRootFromTest(t)
	script := filepath.Join(root, "scripts", "ci", "ruleset-drift.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("ruleset-drift.sh not found at %s: %v", script, err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = root
	env := []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_RULES=" + rules,
	}
	if ghFails {
		env = append(env, "FAKE_GH_FAILS=1")
	}
	// Baseline overrides, when a test set them (t.Setenv reaches os.Getenv here).
	// FAKE_ENFORCEMENT drives the fake gh's first line; RULESET_DRIFT_ENFORCEMENT
	// drives what the script expects it to be.
	for _, k := range []string{
		"RULESET_DRIFT_EXPECTED",
		"RULESET_DRIFT_KNOWN_ABSENT",
		"RULESET_DRIFT_ENFORCEMENT",
		"FAKE_ENFORCEMENT",
	} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env

	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running ruleset-drift.sh: %v\noutput:\n%s", err, out.String())
	}
	t.Logf("exit=%d\n%s", code, out.String())
	return out.String(), code
}

// currentRules is what the live ruleset reports today -- all five expected
// rules, merge_queue included since it was re-enabled on 2026-08-14.
const currentRules = "deletion merge_queue non_fast_forward pull_request required_status_checks"

// TestDriftPassesAndWarnsOnAKnownGap covers the arm whose handling was the whole
// design question -- an expected rule recorded as absent must PASS and WARN.
//
// It drives a SYNTHETIC baseline, because the production one is now empty:
// merge_queue was recorded absent from 2026-08-06 until it was re-enabled on
// 2026-08-14. Leaving this untested because the repository happens to have no
// gap today would mean the mechanism is dead code the next time one appears --
// which is precisely when nobody would notice it had broken.
func TestDriftPassesAndWarnsOnAKnownGap(t *testing.T) {
	// EXPECTED and KNOWN_ABSENT are DISJOINT -- a rule intended but currently
	// missing belongs only in the second. An earlier draft of this test listed
	// it in both and got a FAIL and a WARN for the same rule, which is the
	// script correctly refusing an incoherent baseline.
	t.Setenv("RULESET_DRIFT_EXPECTED", "deletion\npull_request")
	t.Setenv("RULESET_DRIFT_KNOWN_ABSENT", "some_future_rule|memql#3836 -- illustrative gap")

	out, code := runDrift(t, "deletion pull_request", false)

	if code != 0 {
		t.Errorf("exit %d, want 0. A known, recorded gap must not fail the check: a "+
			"scheduled check that is permanently red is a check people mute, which is "+
			"how the gap survives a second time (memql#3836).\n%s", code, out)
	}
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "some_future_rule") {
		t.Errorf("the run does not WARN about the absent rule. Passing "+
			"SILENTLY over the defect the check was built for is the other failure "+
			"mode, and the one that looks like success.\n%s", out)
	}
	if !strings.Contains(out, "memql#3836") {
		t.Errorf("the warning does not name the issue that will close the gap, so a "+
			"reader has the finding and no route to the fix\n%s", out)
	}
	// The coverage line, which is the acceptance criterion: "no drift" alone
	// reads as a clean bill of health over a ruleset missing the queue.
	if !strings.Contains(out, "known-absent") {
		t.Errorf("the run does not report its own coverage. \"no drift\" is a claim about "+
			"the baseline, not about the ruleset, unless the output says how much of "+
			"the ruleset the baseline excuses.\n%s", out)
	}
}

// TestDriftFailsWhenAnExpectedRuleGoes is the 2026-08-06 event, replayed.
func TestDriftFailsWhenAnExpectedRuleGoes(t *testing.T) {
	// required_status_checks removed -- exactly what happened, alongside
	// merge_queue, in the edit that started this.
	out, code := runDrift(t, "deletion non_fast_forward pull_request", false)

	if code == 0 {
		t.Errorf("exit 0 with required_status_checks MISSING. This is the event the check "+
			"exists for (memql#3836).\n%s", out)
	}
	if !strings.Contains(out, "required_status_checks") {
		t.Errorf("the failure does not name the missing rule\n%s", out)
	}
	// The remedy matters here more than usual: the natural fix -- PATCH the one
	// rule back -- deletes everything else, because PATCH replaces the array.
	if !strings.Contains(out, "re-send") {
		t.Errorf("the failure does not warn that a ruleset PATCH REPLACES the rules array. "+
			"Without that, the obvious repair drops every rule it does not mention, "+
			"which is how this happened in the first place.\n%s", out)
	}
}

// TestDriftFailsWhenAKnownGapIsClosed is the arm that keeps the baseline from
// rotting into fiction.
//
// If merge_queue comes back and KNOWN_ABSENT still excuses it, the check goes
// on reporting a gap that no longer exists and -- worse -- never starts
// ENFORCING the rule, so a second removal would be excused too.
func TestDriftFailsWhenAKnownGapIsClosed(t *testing.T) {
	// This is not hypothetical: it is exactly what the live check did on
	// 2026-08-14, the moment merge_queue came back while the baseline still
	// excused it. Driven synthetically here so the arm stays covered now that
	// the real baseline has been updated.
	t.Setenv("RULESET_DRIFT_EXPECTED", "deletion\npull_request")
	t.Setenv("RULESET_DRIFT_KNOWN_ABSENT", "some_future_rule|illustrative gap")

	out, code := runDrift(t, "deletion pull_request some_future_rule", false)

	if code == 0 {
		t.Errorf("exit 0 with a rule PRESENT while still recorded as known-absent. "+
			"The baseline is now wrong, and left alone it would excuse the rule's next "+
			"removal as well.\n%s", out)
	}
	if !strings.Contains(out, "KNOWN_ABSENT") || !strings.Contains(out, "EXPECTED") {
		t.Errorf("the failure does not say how to fix the baseline (move the rule from "+
			"KNOWN_ABSENT to EXPECTED)\n%s", out)
	}
}

// TestDriftDistinguishesUnreadableFromClean is the one that is not about drift.
//
// A token problem, a network failure, a renamed ruleset -- any of these means
// the check learned nothing. Reporting that as "no drift" is the exact failure
// this repository keeps finding: a null result presented as a negative one.
func TestDriftDistinguishesUnreadableFromClean(t *testing.T) {
	out, code := runDrift(t, "", true)

	if code == 0 {
		t.Errorf("exit 0 when the ruleset could not be READ. \"I could not ask\" is not "+
			"\"nothing has changed\".\n%s", out)
	}
	if code == 1 {
		t.Errorf("exit 1 for an unreadable ruleset collides with the drift exit code, so "+
			"a caller cannot tell a real regression from a broken token\n%s", out)
	}
	// Assert on the SUCCESS LINE, not on the substring "no drift".
	//
	// The first draft of this test matched "no drift" and failed -- because the
	// script's own failure message says the result "must not be read as 'no
	// drift'". The phrase appears in two opposite meanings, so matching it
	// tested the wording rather than the claim. `ruleset-drift: OK` is emitted
	// only on the success path and cannot mean anything else.
	if strings.Contains(out, "ruleset-drift: OK") {
		t.Errorf("the output reports success after failing to read the ruleset\n%s", out)
	}
}

// --- enforcement, the second axis (memql#3837) -------------------------------
//
// A ruleset has a rule LIST and an on/off ENFORCEMENT state. They fail
// independently, and the failure that motivated this axis is the one where
// membership stays TRUE: ruleset 19450314 carries `copilot_code_review` and is
// switched off, so the review stopped being requested with nothing going red.
//
// The tests below are the mirror image of the membership ones above. What makes
// them worth writing separately is that each asserts the axis is not being
// answered by the other -- a single verdict over both would pass half of each.

// TestEnforcementFailsWhenTheRulesetIsSwitchedOff is the 19450314 event: every
// expected rule present, and the ruleset not enforcing any of them.
//
// If this passed, the check would be green over the exact defect it was added
// for -- which is what the membership-only check WAS, correctly and by design,
// before this axis existed.
func TestEnforcementFailsWhenTheRulesetIsSwitchedOff(t *testing.T) {
	t.Setenv("RULESET_DRIFT_EXPECTED", "copilot_code_review")
	t.Setenv("RULESET_DRIFT_ENFORCEMENT", "active")
	t.Setenv("FAKE_ENFORCEMENT", "disabled")

	out, code := runDrift(t, "copilot_code_review", false)

	if code == 0 {
		t.Errorf("exit 0 for a ruleset carrying every expected rule and enforcing NONE of "+
			"them. Membership being true is not the ruleset doing anything (memql#3837)."+
			"\n%s", out)
	}
	if !strings.Contains(out, "enforcement") {
		t.Errorf("the failure does not name enforcement, so a reader sees a drift failure "+
			"over a rule list that is in fact correct\n%s", out)
	}
}

// TestEnforcementPassesWhenDisabledIsTheRecordedBaseline is the decision half.
//
// `disabled` on 19450314 is a RECORDED DECISION (docs/internal/ops/ruleset-baseline.md),
// not a gap being tolerated, so a baseline saying `disabled` over a ruleset that
// IS disabled must pass -- otherwise the job is permanently red, and a
// permanently red scheduled check is a muted one, which is how the merge queue
// stayed lost for eight days.
func TestEnforcementPassesWhenDisabledIsTheRecordedBaseline(t *testing.T) {
	t.Setenv("RULESET_DRIFT_EXPECTED", "copilot_code_review")
	t.Setenv("RULESET_DRIFT_ENFORCEMENT", "disabled")
	t.Setenv("FAKE_ENFORCEMENT", "disabled")

	out, code := runDrift(t, "copilot_code_review", false)

	if code != 0 {
		t.Errorf("exit %d for a ruleset in exactly the state the baseline records. A "+
			"scheduled check that is permanently red is a check people mute.\n%s", code, out)
	}
	if !strings.Contains(out, "enforcement disabled") {
		t.Errorf("the coverage line does not state the enforcement state it asserted, so "+
			"the pass reads as a clean bill of health over a ruleset that is switched "+
			"OFF -- the same misreading as \"no drift\" over a missing queue\n%s", out)
	}
}

// TestEnforcementFailsWhenARecordedDecisionIsReversed is the arm that keeps the
// decision record honest, and the reason `disabled` is asserted rather than
// merely tolerated.
//
// Re-enabling 19450314 would be GOOD NEWS and must still fail: the recorded
// decision no longer describes the repository. This is the exact symmetry of
// TestDriftFailsWhenAKnownGapIsClosed -- a baseline that silently accepts the
// state changing back is a baseline asserting nothing.
func TestEnforcementFailsWhenARecordedDecisionIsReversed(t *testing.T) {
	t.Setenv("RULESET_DRIFT_EXPECTED", "copilot_code_review")
	t.Setenv("RULESET_DRIFT_ENFORCEMENT", "disabled")
	t.Setenv("FAKE_ENFORCEMENT", "active")

	out, code := runDrift(t, "copilot_code_review", false)

	if code == 0 {
		t.Errorf("exit 0 after the recorded decision was reversed. The point of recording "+
			"'disabled' as a decision is that changing it is a NEW decision, which "+
			"nothing would prompt if flipping it back were silently accepted.\n%s", out)
	}
	if !strings.Contains(out, "ruleset-baseline.md") {
		t.Errorf("the failure does not point at the decision record, so whoever hits it "+
			"has the finding and no route to the choice it contradicts\n%s", out)
	}
}
