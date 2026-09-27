package dev

// npm_cache_lanes_test.go -- every Node lane caches npm, keyed on what it
// installs, and none retries a test (memql#5482).
//
// No Node lane cached npm, so every install was a cold download from the
// registry, and one lane retried its whole TEST target only because the
// installs inside it were a network call. With the cache the installs come
// from the runner, and the retry that hid a flaky test is gone.

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type nodeStep struct {
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

func nodeJobs(t *testing.T) map[string][]nodeStep {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Steps []nodeStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ciYAML(t), &wf); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	out := map[string][]nodeStep{}
	for name, job := range wf.Jobs {
		out[name] = job.Steps
	}
	return out
}

// TestEverySetupNodeCachesNpm: cache: npm, and a dependency path naming only
// tracked lockfiles -- a path to a file that does not exist hashes to a
// constant key, which is a cache that never invalidates.
func TestEverySetupNodeCachesNpm(t *testing.T) {
	tracked := map[string]bool{}
	out, err := exec.Command("git", "-C", repoRoot(t), "ls-files", "*package-lock.json").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, f := range strings.Fields(string(out)) {
		tracked[f] = true
	}
	lanes := 0
	for job, steps := range nodeJobs(t) {
		for _, s := range steps {
			if !strings.HasPrefix(s.Uses, "actions/setup-node@") {
				continue
			}
			lanes++
			if s.With["cache"] != "npm" {
				t.Errorf("job %q: actions/setup-node sets no `cache: npm`, so every install downloads cold", job)
				continue
			}
			paths, _ := s.With["cache-dependency-path"].(string)
			if strings.TrimSpace(paths) == "" {
				t.Errorf("job %q: `cache: npm` with no cache-dependency-path keys on a root lockfile this repo does not have", job)
			}
			for _, p := range strings.Fields(paths) {
				if !tracked[p] {
					t.Errorf("job %q: cache-dependency-path names %q, which is not a tracked package-lock.json", job, p)
				}
			}
		}
	}
	if lanes < 5 {
		t.Errorf("found %d setup-node steps in ci.yml, want the five Node lanes; this guard is reading less than it should", lanes)
	}
}

// retryWraps matches a scripts/ci/retry.sh invocation and captures what it runs.
var retryWraps = regexp.MustCompile(`scripts/ci/retry\.sh\s+--\s+(.*)$`)

// testCommand matches a command that runs tests.
var testCommand = regexp.MustCompile(`(^|\s)(go test|npm test|npm run test)(\s|$)|(^|\s)make\s+\S*-test(\s|$)`)

// TestNoRetryWrapsATest holds retry.sh's own rule in the one file that calls
// it: retry network operations, never a test -- a retried flake is invisible.
func TestNoRetryWrapsATest(t *testing.T) {
	for job, steps := range nodeJobs(t) {
		for _, s := range steps {
			for _, line := range commandLines(s.Run) {
				m := retryWraps.FindStringSubmatch(line)
				if m != nil && testCommand.MatchString(m[1]) {
					t.Errorf("job %q retries a test (%q). scripts/ci/retry.sh is for network operations; a "+
						"retried test is a flake nobody sees (memql#5482).", job, line)
				}
			}
		}
	}
}
