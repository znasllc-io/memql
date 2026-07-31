// Static guard over the gitleaks workflow's scan scoping
// (znasllc-io/memql#2996).
//
// Background: the full-history lane runs `gitleaks git .` on a fetch-depth:0
// checkout. That checkout fetches EVERY branch, and gitleaks defaults to
// walking every ref it can reach (`--all`) -- not just the commit under test.
// The consequence is not obvious and it is severe: two synthetic high-entropy
// fixtures on a single unmerged branch failed every merge candidate AND every
// push to main, regardless of what those candidates contained. Six consecutive
// candidates across three unrelated PRs were evicted and main went red, none of
// them containing the branch at fault.
//
// Scoping the walk to `HEAD --tags` keeps it a full-history walk -- every commit
// reachable from the candidate is still read, so added-then-removed secrets are
// still caught -- while dropping refs that are not the candidate.
//
// This is the second occurrence of the same wedge (#1539 allowlisted the
// offending fixture but left the ref-scoping alone, so it came back with the
// next one). These assertions exist so a third cannot happen silently.
//
// They deliberately assert on MEANING rather than spelling: an earlier version
// of this guard rejected harmless reformatting while waving through mutations
// that re-opened the bug outright.
package dev

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/dev/ -> repo root is two directories up.
	parts := append([]string{filepath.Dir(thisFile), "..", ".."}, rel...)
	raw, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(raw)
}

// gitleaksGitCommand returns the `gitleaks git` invocation from the workflow.
// It matches the command itself wherever it appears -- a plain `run:`, a block
// scalar, any indentation -- so reformatting the step does not fail the guard.
// Comment lines are skipped so the flag cannot be "satisfied" by prose.
func gitleaksGitCommand(t *testing.T) string {
	t.Helper()
	wf := repoFile(t, ".github", "workflows", "gitleaks.yml")
	for _, line := range strings.Split(wf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "gitleaks git") {
			continue
		}
		return trimmed
	}
	t.Fatal("no `gitleaks git` command found in .github/workflows/gitleaks.yml")
	return ""
}

var logOptsRe = regexp.MustCompile(`--log-opts=("([^"]*)"|'([^']*)'|(\S+))`)

func TestGitleaksFullHistoryScanIsRefScoped(t *testing.T) {
	cmd := gitleaksGitCommand(t)

	// pflag takes the LAST occurrence, so a second --log-opts silently wins.
	// Appending `--log-opts="--all"` restores the unscoped walk while every
	// naive "does it contain the flag" assertion stays green.
	if n := strings.Count(cmd, "--log-opts"); n != 1 {
		t.Fatalf("expected exactly one --log-opts (pflag honours only the last, "+
			"so a second one silently restores the unscoped walk); found %d in: %s", n, cmd)
	}

	m := logOptsRe.FindStringSubmatch(cmd)
	if m == nil {
		t.Fatalf("full-history scan must pass --log-opts to scope the walk; "+
			"without it a fixture on ANY unmerged branch fails every merge "+
			"candidate and every push to main (#2996).\ngot: %s", cmd)
	}
	// Group 2/3 are the quoted bodies; group 4 is the bare form.
	value := m[2] + m[3] + m[4]
	fields := strings.Fields(value)

	// The value is two tokens, so the quotes are load-bearing: unquoted,
	// `--tags` would be parsed as an argument to gitleaks rather than git log.
	if m[4] != "" {
		t.Errorf("--log-opts value must be quoted -- it carries two tokens, and "+
			"unquoted the shell hands `--tags` to gitleaks instead of git log.\ngot: %s", cmd)
	}

	var hasHead, hasTags bool
	for _, f := range fields {
		switch f {
		case "HEAD":
			hasHead = true
		case "--tags":
			hasTags = true
		case "--all":
			t.Errorf("--log-opts must not pass --all: that is the unscoped default "+
				"this guard exists to prevent (#2996).\ngot: %s", cmd)
		}
	}
	if !hasHead {
		t.Errorf("--log-opts must scope the walk to HEAD (the ancestry under test).\ngot: %s", cmd)
	}
	// Without --tags, a tag that is not an ancestor of main falls out of scope
	// and its history goes permanently unscanned.
	if !hasTags {
		t.Errorf("--log-opts must include --tags: scoping to HEAD alone drops any "+
			"tag that is not an ancestor of main, leaving that history unscanned "+
			"forever (this repo has one such tag).\ngot: %s", cmd)
	}
}

// The fast PR lane is deliberately a working-tree scan; swapping it to the
// history walk is what makes PRs expensive, and swapping it away entirely
// removes the only per-PR secret gate.
func TestGitleaksPRLaneStaysWorkingTreeScan(t *testing.T) {
	wf := repoFile(t, ".github", "workflows", "gitleaks.yml")
	var found bool
	for _, line := range strings.Split(wf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "gitleaks dir") {
			found = true
		}
	}
	if !found {
		t.Error("PR lane must keep the fast working-tree scan (`gitleaks dir .`)")
	}
}
