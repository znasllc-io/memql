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

// gitleaksGitCommand returns the workflow's single `gitleaks git` invocation.
// It matches the command wherever it appears -- a plain `run:`, a block scalar,
// any indentation -- so reformatting the step does not fail the guard, and
// comment lines are skipped so no assertion can be satisfied by prose.
//
// It collects EVERY occurrence rather than the first. Returning the first would
// miss a second invocation on a later line of a block scalar:
//
//	run: |
//	  gitleaks git . --no-banner --log-opts="HEAD --tags"
//	  gitleaks git . --no-banner --log-opts="--all"
//
// Both run, the second restores the unscoped walk, and every per-line assertion
// below would still pass.
func gitleaksGitCommand(t *testing.T) string {
	t.Helper()
	wf := repoFile(t, ".github", "workflows", "gitleaks.yml")
	var found []string
	for _, line := range strings.Split(wf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "gitleaks git") {
			continue
		}
		found = append(found, trimmed)
	}
	switch len(found) {
	case 0:
		t.Fatal("no `gitleaks git` command found in .github/workflows/gitleaks.yml")
	case 1:
	default:
		t.Fatalf("expected exactly one `gitleaks git` invocation; a second one "+
			"restores the unscoped walk regardless of how the first is scoped.\ngot: %v", found)
	}
	return found[0]
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

	// Allow-list the value rather than blacklisting --all. A blacklist stops
	// only the narrowing it happens to name: --log-opts="HEAD --tags -n 1"
	// scans a single commit and would sail past a check that merely rejects
	// --all and requires HEAD.
	want := map[string]bool{"HEAD": true, "--tags": true}
	seen := map[string]bool{}
	for _, f := range fields {
		if !want[f] {
			t.Errorf("unexpected token %q in --log-opts. The walk must be scoped to "+
				"exactly `HEAD --tags`; anything else either widens it back to the "+
				"unscoped default or narrows how much history is read (#2996).\ngot: %s", f, cmd)
			continue
		}
		seen[f] = true
	}
	if !seen["HEAD"] {
		t.Errorf("--log-opts must scope the walk to HEAD (the ancestry under test).\ngot: %s", cmd)
	}
	// Without --tags, a tag that is not an ancestor of main falls out of scope
	// and its history goes permanently unscanned.
	if !seen["--tags"] {
		t.Errorf("--log-opts must include --tags: scoping to HEAD alone drops any "+
			"tag that is not an ancestor of main, leaving that history unscanned "+
			"forever (this repo has one such tag).\ngot: %s", cmd)
	}

	// Shell expansion would let the effective flags differ from the literal
	// ones: `... --log-opts="HEAD --tags" $EXTRA` with EXTRA=--log-opts=--all
	// reads as correctly scoped here while running unscoped.
	if strings.Contains(cmd, "$") {
		t.Errorf("gitleaks command must not interpolate shell variables -- the "+
			"effective scope would no longer be readable from the workflow.\ngot: %s", cmd)
	}
	// A scan whose failure is swallowed is not a gate.
	for _, esc := range []string{"|| true", "--exit-code 0", "--exit-code=0"} {
		if strings.Contains(cmd, esc) {
			t.Errorf("gitleaks command must not suppress its failure exit (%q).\ngot: %s", esc, cmd)
		}
	}
}

// The scope flags are meaningless if the checkout has no history to walk.
// Flipping fetch-depth to 1 leaves every assertion above green while the scan
// covers exactly one commit and reports success -- the same class of silent
// under-coverage this guard exists to prevent, and invisible in review.
func TestGitleaksFullHistoryCheckoutIsUnshallow(t *testing.T) {
	wf := repoFile(t, ".github", "workflows", "gitleaks.yml")
	if !regexp.MustCompile(`(?m)^\s*fetch-depth:\s*0\s*$`).MatchString(wf) {
		t.Error("the non-PR checkout must set `fetch-depth: 0`; without full history " +
			"`gitleaks git` scans a single commit and passes trivially (#2996)")
	}
}

// The full-history lane must actually run on the events it claims to cover.
// `scan` is a required check, so a step gated off by a narrowed `if:` reports
// green having scanned nothing.
func TestGitleaksFullHistoryLaneRunsOnAllNonPREvents(t *testing.T) {
	wf := repoFile(t, ".github", "workflows", "gitleaks.yml")
	if !strings.Contains(wf, "if: github.event_name != 'pull_request'") {
		t.Error("the full-history lane must stay gated on " +
			"`github.event_name != 'pull_request'` so it covers push, merge_group " +
			"and schedule; narrowing it silently drops merge-queue coverage (#2996)")
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
