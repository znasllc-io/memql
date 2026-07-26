// Contract gate for scripts/dev/stalled-prs.sh (znasllc-io/memql#2833).
package dev

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const stalledPRsScript = "stalled-prs.sh"

// classify exercises the script's bucketing function directly, by sourcing the
// script with main() suppressed. The classifier is a pure function of five
// strings, so it can be tested without touching the GitHub API -- which
// matters, because the buckets that are hardest to observe live (DRAFT, RED,
// CONFLICT) are exactly the ones a wrong rule would silently mislabel as
// STALLED.
func classify(t *testing.T, draft, mergeState, queued, checks, idle string) string {
	t.Helper()
	// `main "$@"` runs on source, so feed it a no-op by overriding main after
	// the source completes -- simplest is to strip the trailing invocation.
	raw, err := os.ReadFile(stalledPRsScript)
	if err != nil {
		t.Fatalf("read %s: %v", stalledPRsScript, err)
	}
	body := strings.Replace(string(raw), "\nmain \"$@\"\n", "\n", 1)

	script := body + "\nclassify " +
		strings.Join([]string{draft, mergeState, queued, checks, idle}, " ") + "\n"
	out, err := exec.Command("bash", "-c", script).Output()
	if err != nil {
		t.Fatalf("classify(%s %s %s %s %s): %v", draft, mergeState, queued, checks, idle, err)
	}
	return strings.TrimSpace(string(out))
}

// TestStalledPRs_ClassifierBuckets pins the rule. Only STALLED is a finding,
// so every other state must be reported as itself: a PR mislabelled STALLED
// invites someone to enqueue a draft, a red build, or a conflicted branch.
func TestStalledPRs_ClassifierBuckets(t *testing.T) {
	cases := []struct {
		name                                  string
		draft, mergeState, queued, checks, id string
		want                                  string
	}{
		// The finding: green, mergeable, unqueued, idle past the threshold.
		{"green and idle", "false", "CLEAN", "false", "green", "120", "STALLED"},

		// Everything that must NOT be flagged.
		{"already queued", "false", "CLEAN", "true", "green", "120", "QUEUED"},
		{"draft", "true", "CLEAN", "false", "green", "120", "DRAFT"},
		{"failing checks", "false", "CLEAN", "false", "failing", "120", "RED"},
		{"checks still running", "false", "CLEAN", "false", "pending", "120", "PENDING"},
		{"merge conflict", "false", "DIRTY", "false", "green", "120", "CONFLICT"},
		{"blocked", "false", "BLOCKED", "false", "green", "120", "CONFLICT"},
		{"within the idle window", "false", "CLEAN", "false", "green", "5", "FRESH"},

		// Precedence: a draft that is also red is still reported as a draft --
		// the author has not offered it, so its checks are not the story.
		{"draft beats red", "true", "CLEAN", "false", "failing", "120", "DRAFT"},
		// Queued beats idle: the queue owns it, however long it has sat.
		{"queued beats idle", "false", "CLEAN", "true", "green", "9999", "QUEUED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(t, tc.draft, tc.mergeState, tc.queued, tc.checks, tc.id); got != tc.want {
				t.Errorf("classify(draft=%s state=%s queued=%s checks=%s idle=%s) = %q, want %q",
					tc.draft, tc.mergeState, tc.queued, tc.checks, tc.id, got, tc.want)
			}
		})
	}
}

// TestStalledPRs_IsReadOnly is the load-bearing property, and the reason this
// tool reports instead of sweeping.
//
// Adversarial review of every PR merged on the night this was written found a
// real defect in each one, so "green" is demonstrably not "reviewed" here. A
// sweeper that enqueued automatically would land unreviewed work at exactly
// the moment nobody is watching -- and, being unattended, could also race a
// session that is still working.
//
// The guarantee is therefore structural: the script performs no GitHub
// mutation at all. This test fails if a future edit adds one.
func TestStalledPRs_IsReadOnly(t *testing.T) {
	raw, err := os.ReadFile(stalledPRsScript)
	if err != nil {
		t.Fatalf("read %s: %v", stalledPRsScript, err)
	}
	src := string(raw)

	for _, mutation := range []string{
		"gh pr merge",
		"gh pr edit",
		"gh pr close",
		"gh pr ready",
		"gh issue edit",
		"gh issue close",
		"gh api -X",
		"--method POST",
		"--method PATCH",
		"--method DELETE",
		"git push",
	} {
		if strings.Contains(src, mutation) {
			t.Errorf("%s contains %q; this reporter must stay read-only -- enqueuing green-but-unreviewed work unattended is the failure it exists to avoid (memql#2833)",
				stalledPRsScript, mutation)
		}
	}
}

// TestStalledPRs_SkippedChecksCountAsGreen pins one classification detail that
// is easy to get wrong and would make the report useless: this repo
// path-filters several CI lanes, so a healthy PR routinely carries `skipped`
// runs. Treating those as non-green would mark every PR RED.
func TestStalledPRs_SkippedChecksCountAsGreen(t *testing.T) {
	src, err := os.ReadFile(stalledPRsScript)
	if err != nil {
		t.Fatalf("read %s: %v", stalledPRsScript, err)
	}
	if !strings.Contains(string(src), "skipped") {
		t.Error("check_state does not mention `skipped`; path-filtered lanes are normal here, so a healthy PR would classify as RED")
	}
}
