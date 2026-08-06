// Static guard: every path filter names something that exists
// (znasllc-io/memql#3163).
//
// # The failure mode
//
// `dorny/paths-filter` does not error on a pattern that matches nothing. It
// simply reports the bucket as false, forever. So a bucket pointing at a
// directory that was renamed or never existed is not a loud failure -- it is a
// lane that silently stops running, on a gate that stays green.
//
// That is the same shape as the fail-opens this repo has already had to close
// twice (memql#2972: 383 files matched no bucket at all; memql#3019: an empty
// `needs` made every lane advisory at once). The pattern is consistent enough
// to be worth guarding structurally rather than hoping review catches it.
//
// This matters more than usual for the target-module buckets added in #3163,
// because they are NOT yet consumed by any lane's `if:`. An unconsumed bucket
// gets no smoke test from CI going red -- nothing would notice a typo until
// someone wires it up during the module split and quietly gets a lane that
// never runs.
package ci

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func bucketsRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// bucketLiteralPrefix reduces a glob to the longest leading path segment run
// that contains no glob metacharacter, which is the part that must exist on
// disk. `component/memql/**` -> `component/memql`; `**/*.md` -> "" (skipped).
func bucketLiteralPrefix(pattern string) string {
	pattern = strings.TrimPrefix(pattern, "!")
	var kept []string
	for _, seg := range strings.Split(pattern, "/") {
		if strings.ContainsAny(seg, "*?[]{}") {
			break
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "/")
}

// TestEveryFilterBucketPathExists is the guard.
func TestEveryFilterBucketPathExists(t *testing.T) {
	root := bucketsRepoRoot(t)

	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				With struct {
					Filters string `yaml:"filters"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}

	var filtersYAML string
	for _, s := range wf.Jobs["changes"].Steps {
		if s.With.Filters != "" {
			filtersYAML = s.With.Filters
			break
		}
	}
	if filtersYAML == "" {
		t.Fatal("no `filters:` found on the `changes` job -- this guard cannot verify " +
			"what it was written to verify, so it fails closed")
	}

	var buckets map[string][]string
	if err := yaml.Unmarshal([]byte(filtersYAML), &buckets); err != nil {
		t.Fatalf("parse the changes job's filters block: %v", err)
	}
	if len(buckets) == 0 {
		t.Fatal("parsed zero filter buckets")
	}

	for name, patterns := range buckets {
		for _, pattern := range patterns {
			prefix := bucketLiteralPrefix(pattern)
			if prefix == "" {
				continue // a pure glob like `**/*.md` names no fixed path
			}
			// An entirely literal pattern names a FILE, and a file is allowed
			// not to exist yet: the `go` bucket legitimately watches `go.work`
			// and `go.work.sum` before the module split creates them, and that
			// is the correct way to write "react if this appears". Only a
			// pattern with a glob after its prefix (`component/mcp/**`) is
			// asserting that a directory exists to be globbed.
			if prefix == strings.TrimPrefix(pattern, "!") {
				continue
			}
			if _, err := os.Stat(filepath.Join(root, prefix)); err != nil {
				t.Errorf("filter bucket %q names %q, whose literal prefix %q does not exist.\n"+
					"dorny/paths-filter does not error on a pattern that matches nothing -- it "+
					"reports the bucket false forever, so the lanes it gates silently stop "+
					"running while the gate stays green (the memql#2972 / memql#3019 shape).",
					name, pattern, prefix)
			}
		}
	}
}
