// Static guard over the gitleaks workflow's scan scoping and the allowlist
// that depends on it (znasllc-io/memql#2996).
//
// Background: the full-history lane runs `gitleaks git .` on a fetch-depth:0
// checkout. That checkout fetches EVERY branch, and gitleaks defaults to
// walking every ref it can reach -- not just the commit under test. The
// consequence is not obvious and it is severe: two synthetic 64-char hex
// fixtures on a single unmerged branch failed every merge candidate AND every
// push to main, regardless of what those candidates contained. Six consecutive
// candidates across three unrelated PRs were evicted and main went red, none of
// which included the offending branch.
//
// `--log-opts="HEAD"` scopes the walk to the ancestry actually under test while
// keeping it a full-history walk, so added-then-removed secrets are still
// caught. Measured when the flag landed: unscoped 2145 commits, scoped 2124 --
// the delta being exactly the unmerged branch.
//
// This is the second occurrence of the same wedge (#1539 allowlisted a fixture
// but left the ref-scoping alone, so it recurred). These assertions exist so a
// third one cannot happen silently.
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
	p := filepath.Join(parts...)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(raw)
}

// The full-history lane must stay scoped to the ancestry under test. Dropping
// the flag re-opens the repo-wide wedge described above.
func TestGitleaksFullHistoryScanIsRefScoped(t *testing.T) {
	wf := repoFile(t, ".github", "workflows", "gitleaks.yml")

	// Locate the command itself rather than asserting on the whole file, so
	// the flag cannot be "satisfied" by an unrelated mention in a comment.
	var scanCmd string
	for _, line := range strings.Split(wf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "run:") && strings.Contains(trimmed, "gitleaks git") {
			scanCmd = trimmed
			break
		}
	}
	if scanCmd == "" {
		t.Fatal("no `gitleaks git` run step found in gitleaks.yml")
	}
	if !strings.Contains(scanCmd, `--log-opts="HEAD"`) {
		t.Errorf(`full-history scan must pass --log-opts="HEAD" to scope the walk to `+
			`the candidate's own ancestry; without it a fixture on ANY unmerged branch `+
			"fails every merge candidate and every push to main (#2996).\ngot: %s", scanCmd)
	}

	// The fast PR lane is deliberately a working-tree scan and must not be
	// swapped to the history walk -- that is what keeps PRs cheap.
	if !strings.Contains(wf, "gitleaks dir .") {
		t.Error("PR lane must keep the fast working-tree scan (`gitleaks dir .`)")
	}
}

// The k3d master-key guard fixtures must stay allowlisted, and -- the part that
// actually bites -- the allowlist must target the extracted SECRET. Setting
// regexTarget = "match" makes an anchored shape regex silently never fire,
// because "match" carries the surrounding `name = "..."` context.
func TestGitleaksAllowlistsK3dHexFixtures(t *testing.T) {
	cfg := repoFile(t, ".gitleaks.toml")

	blocks := strings.Split(cfg, "[[allowlists]]")
	var found bool
	for _, b := range blocks {
		if !strings.Contains(b, `^scripts/k3d/.*_test\.go$`) {
			continue
		}
		found = true
		if !strings.Contains(b, `^[0-9a-fA-F]{64}$`) {
			t.Error("k3d fixture allowlist must key on the 64-char hex SHAPE, " +
				"so no fixture literal is copied into this config")
		}
		if regexp.MustCompile(`(?m)^\s*regexTarget`).MatchString(b) {
			t.Error(`k3d fixture allowlist must NOT set regexTarget: the regex has to ` +
				`apply to the extracted secret. Against "match" the anchored shape ` +
				`regex never fires and the allowlist silently does nothing (#2996).`)
		}
		if !strings.Contains(b, `condition      = "AND"`) && !strings.Contains(b, `condition = "AND"`) {
			t.Error(`k3d fixture allowlist must use condition = "AND" so the same ` +
				"shape outside a test file still trips the scanner")
		}
	}
	if !found {
		t.Error(`.gitleaks.toml must allowlist the scripts/k3d/*_test.go master-key ` +
			"fixtures (#2958); without it those commits fail the full-history scan " +
			"once they reach main (#2996)")
	}
}
