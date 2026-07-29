package scan

// tags_test.go -- the CI drift gate for BUILD-TAGGED TEST SUITES (memql#2903).
//
// The untagged `go test ./...` never RUNS a //go:build-tagged test file, so a
// tagged suite is CI-invisible unless some lane runs it with `-tags`. That fact
// was recorded in .github/workflows/ci.yml as a hand-maintained audit comment
// listing which tags carry test files and which lane covers each.
//
// The comment drifted, on three tags at once:
//
//	voice     -- "no tagged *_test.go files"; there is one
//	identity  -- listed among tags that carry no test files; it has one
//	mcp       -- absent entirely; it has three
//
// That is the "two copies that agree until they don't" shape this repo keeps
// getting bitten by (#2815, #2863, #2852, #2872, #2896). It bit memql#2888
// concretely: the seam that vulnerability lives on is in an //go:build mcp
// file, so a regression test placed beside it would never have run.
//
// This replaces the comment with a gate. It reads BOTH sides -- the tags
// actually present on *_test.go files, and the `go test -tags X` invocations
// actually present in ci.yml -- so neither can drift without turning a lane
// red. A second hand-maintained list would have reproduced the original defect.
//
// Untagged on purpose: this test must run in the default `go test ./...` lane,
// because it is the thing that catches tags which do not.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// deliberatelyNotRunInCI maps a build tag to WHY no CI lane runs its tests.
//
// An entry here is a considered decision, not a parking space: each needs a
// reason that would survive review. A tag that is merely inconvenient does not
// belong here -- it belongs in a lane.
var deliberatelyNotRunInCI = map[string]string{
	"clustere2e":  "needs a provisioned parity cluster (k3d + ArgoCD); covered by the parity-cluster e2e workflow, not by this repo's CI",
	"telnyx_live": "hits the live Telnyx API with real credentials; must not run on every PR",
}

// buildTagLine matches a //go:build line and captures its expression.
var buildTagLine = regexp.MustCompile(`^//go:build\s+(.+)$`)

// goTestTags matches a `go test ... -tags X` invocation in the workflow.
var goTestTags = regexp.MustCompile(`go test\s+(?:[^\n]*?\s)?-tags[= ]([A-Za-z0-9_,]+)`)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod walking up from %s", filepath.Dir(file))
		}
		dir = parent
	}
}

// taggedTestFiles maps each POSITIVE build tag to the test files carrying it.
//
// Negated terms (`!planner`) are skipped deliberately: a file gated on `!x`
// compiles in the DEFAULT build, so the untagged lane already runs it. Only a
// positive tag can hide a test from CI, which is the failure this gate exists
// to catch.
func taggedTestFiles(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "package ") {
				break // build constraints precede the package clause
			}
			m := buildTagLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			for _, term := range regexp.MustCompile(`[^A-Za-z0-9_!]+`).Split(m[1], -1) {
				term = strings.TrimSpace(term)
				if term == "" || strings.HasPrefix(term, "!") {
					continue
				}
				out[term] = append(out[term], rel)
			}
			break
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	return out
}

// tagsRunByCI extracts every tag named in a `go test -tags ...` invocation
// anywhere in .github/workflows/.
func tagsRunByCI(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	dir := filepath.Join(root, ".github", "workflows")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			t.Fatalf("reading %s: %v", e.Name(), readErr)
		}
		for _, m := range goTestTags.FindAllStringSubmatch(string(data), -1) {
			for _, tag := range strings.Split(m[1], ",") {
				if tag = strings.TrimSpace(tag); tag != "" {
					out[tag] = e.Name()
				}
			}
		}
	}
	return out
}

// TestEveryTaggedTestSuiteHasACILane is the gate.
//
// For every build tag that gates at least one *_test.go, some workflow must run
// `go test -tags <tag>`, or the tag must carry a documented reason in
// deliberatelyNotRunInCI.
func TestEveryTaggedTestSuiteHasACILane(t *testing.T) {
	root := repoRoot(t)
	tagged := taggedTestFiles(t, root)
	ciTags := tagsRunByCI(t, root)

	if len(tagged) == 0 {
		t.Fatal("found no build-tagged test files at all -- the scanner is broken, not the tree")
	}

	tags := make([]string, 0, len(tagged))
	for tag := range tagged {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	for _, tag := range tags {
		files := tagged[tag]
		sort.Strings(files)

		if lane, ok := ciTags[tag]; ok {
			t.Logf("tag %-12s %d test file(s), run by %s", tag, len(files), lane)
			continue
		}
		if reason, ok := deliberatelyNotRunInCI[tag]; ok {
			t.Logf("tag %-12s %d test file(s), deliberately not run: %s", tag, len(files), reason)
			continue
		}
		t.Errorf("build tag %q gates %d test file(s) that NO CI lane runs:\n  %s\n\n"+
			"A //go:build-tagged test is invisible to the untagged `go test ./...`, so these "+
			"assert nothing on any PR. Either add a `go test -tags %s ...` step to a workflow, "+
			"or add %q to deliberatelyNotRunInCI with a reason that would survive review "+
			"(memql#2903).",
			tag, len(files), strings.Join(files, "\n  "), tag, tag)
	}
}

// TestDeliberateExclusionsStillHaveTests keeps the exclusion list honest in the
// other direction: an entry whose tests have all been deleted or un-tagged is
// stale, and a stale exemption is how the next real gap gets waved through.
func TestDeliberateExclusionsStillHaveTests(t *testing.T) {
	root := repoRoot(t)
	tagged := taggedTestFiles(t, root)

	names := make([]string, 0, len(deliberatelyNotRunInCI))
	for tag := range deliberatelyNotRunInCI {
		names = append(names, tag)
	}
	sort.Strings(names)

	for _, tag := range names {
		if len(tagged[tag]) == 0 {
			t.Errorf("deliberatelyNotRunInCI lists %q, but no *_test.go carries that tag any more. "+
				"Remove the entry -- a stale exemption reads as a considered decision and is not one "+
				"(memql#2903).", tag)
		}
	}
}
