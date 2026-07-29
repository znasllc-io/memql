package citags

// tags_test.go -- the CI drift gate for BUILD-TAGGED TEST SUITES (memql#2903).
//
// The untagged `go test ./...` never RUNS a //go:build-tagged test file, so a
// tagged suite is CI-invisible unless some lane runs it with `-tags`. That fact
// used to live in a hand-maintained audit comment in .github/workflows/ci.yml,
// which had drifted on four tags at once: mcp was absent entirely (three files),
// and identity, telnyx_live and voice were all covered by a closing "no other
// build tag carries test files today" that was false of each. It bit memql#2888 concretely: that
// vulnerability's seam is in an //go:build mcp file, so a regression test
// placed beside it would never have run.
//
// # Why go/build rather than a regex
//
// A first cut of this gate scanned for `//go:build` with a line regex and split
// the expression on non-identifier characters. Review demolished it, and the
// failures are worth recording because they are what this file must not
// regress into:
//
//   - blind to a constraint placed after a licence header (it stopped at the
//     first blank line), so a whole class of tagged file was invisible;
//   - blind to the legacy `// +build` form, still honoured by the toolchain;
//   - term-split rather than evaluated, so `!(a || b)` -- which builds by
//     DEFAULT -- demanded lanes for a and b, and `a || b` demanded both when
//     either suffices;
//   - no notion of GOOS/GOARCH/cgo, so `//go:build linux` would have demanded a
//     CI lane called "linux".
//
// go/build.Context.MatchFile answers the only question that matters -- "would
// THIS build configuration compile THIS file?" -- and gets constraint
// placement, `// +build`, boolean evaluation and platform tags for free.
//
// # Why YAML rather than a regex
//
// That same first cut grepped ci.yml as raw text. Four of its eight matches
// were step `name:` lines: the gate was satisfied by a LABEL, and deleting
// every `run:` line still passed. It could not tell a lane from its title,
// which is exactly the failure it exists to catch. This parses the YAML and
// reads only `jobs.*.steps[].run`.
//
// # Why package lists, not just tags
//
// Matching tag -> workflow says nothing about whether the lane runs the PACKAGE
// the tagged file lives in. A `//go:build mcp` test under component/grpc/ would
// have been "covered" by a lane running `-tags mcp ./app/ ./component/mcp/`
// while never executing. Now that every node tag has a lane, that is the most
// likely way this bug recurs, so the comparison is tag AND package.
//
// Untagged on purpose: this must run in the default lane, because it is what
// catches the tags that do not.

import (
	"go/build"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deliberatelyNotRunInCI maps a build tag to WHY no CI lane runs its tests.
//
// An entry is a considered decision, not a parking space: each needs a reason
// that would survive review. A tag that is merely inconvenient belongs in a
// lane.
var deliberatelyNotRunInCI = map[string]string{
	"clustere2e":  "needs a provisioned parity cluster (k3d + ArgoCD); covered by the parity-cluster e2e workflow, not by this repo's CI",
	"telnyx_live": "hits the live Telnyx API with real credentials; must not run on every PR",
}

// prCriticalWorkflows are the workflow files whose lanes actually gate a pull
// request. A `go test -tags X` in a workflow that only runs on
// workflow_dispatch or a schedule does NOT protect a PR, so it must not satisfy
// this gate.
var prCriticalWorkflows = map[string]bool{"ci.yml": true}

// goTestCmd matches a `go test` invocation. Applied only to the text of a
// step's `run:` block, never to the whole file.
var goTestCmd = regexp.MustCompile(`go test\b([^\n]*)`)

// tagsFlag pulls the tag list out of one command's arguments.
var tagsFlag = regexp.MustCompile(`-tags[= ]["']?([A-Za-z0-9_,]+)["']?`)

// lane is one `go test -tags ... <packages>` step.
type lane struct {
	tags     []string
	pkgs     []string
	workflow string
}

// covers reports whether this lane's package arguments include dir, a
// repo-relative directory such as "app" or "component/mcp".
func (l lane) covers(dir string) bool {
	for _, p := range l.pkgs {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		p = strings.TrimSuffix(p, "/")
		switch {
		case p == "..." || p == "":
			return true
		case strings.HasSuffix(p, "/..."):
			if base := strings.TrimSuffix(p, "/..."); dir == base || strings.HasPrefix(dir, base+"/") {
				return true
			}
		case p == dir:
			return true
		}
	}
	return false
}

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

// workflowDoc is the sliver of the GitHub Actions schema this gate reads.
type workflowDoc struct {
	Jobs map[string]struct {
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// ciLanes parses the PR-critical workflows and returns every `go test -tags`
// step, reading ONLY steps[].run.
func ciLanes(t *testing.T, root string) []lane {
	t.Helper()
	var out []lane
	dir := filepath.Join(root, ".github", "workflows")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !prCriticalWorkflows[e.Name()] {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			t.Fatalf("reading %s: %v", e.Name(), readErr)
		}
		var wf workflowDoc
		if err := yaml.Unmarshal(data, &wf); err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for _, job := range wf.Jobs {
			for _, step := range job.Steps {
				for _, cmd := range goTestCmd.FindAllStringSubmatch(step.Run, -1) {
					args := cmd[1]
					m := tagsFlag.FindStringSubmatch(args)
					if m == nil {
						continue
					}
					var pkgs []string
					for _, f := range strings.Fields(args) {
						if strings.HasPrefix(f, "./") || f == "..." {
							pkgs = append(pkgs, f)
						}
					}
					out = append(out, lane{tags: strings.Split(m[1], ","), pkgs: pkgs, workflow: e.Name()})
				}
			}
		}
	}
	return out
}

// buildsUnder reports whether the file compiles under the given extra build
// tags, using the real toolchain matcher.
func buildsUnder(t *testing.T, dir, name string, tags []string) bool {
	t.Helper()
	ctx := build.Default
	ctx.BuildTags = tags
	ok, err := ctx.MatchFile(dir, name)
	if err != nil {
		t.Fatalf("MatchFile(%s, %s): %v", dir, name, err)
	}
	return ok
}

// testFiles walks the tree for *_test.go, skipping directories the Go toolchain
// itself ignores.
func testFiles(t *testing.T, root string) [][2]string {
	t.Helper()
	var out [][2]string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (n == "testdata" || n == "node_modules" || n == "vendor" ||
				strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			out = append(out, [2]string{filepath.Dir(path), d.Name()})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	return out
}

// excludedReason reports whether the file is covered by a documented exclusion,
// i.e. it compiles once that excluded tag is supplied.
func excludedReason(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	for tag, reason := range deliberatelyNotRunInCI {
		if buildsUnder(t, dir, name, []string{tag}) {
			return reason, true
		}
	}
	return "", false
}

// TestEveryTaggedTestFileIsRunSomewhere is the gate.
//
// For every *_test.go that does NOT compile in the default build, some
// PR-critical lane must run it -- matching tags AND package -- or its tag must
// carry a documented reason.
func TestEveryTaggedTestFileIsRunSomewhere(t *testing.T) {
	root := repoRoot(t)
	files := testFiles(t, root)
	lanes := ciLanes(t, root)

	if len(files) == 0 {
		t.Fatal("found no *_test.go files at all -- the scanner is broken, not the tree")
	}
	if len(lanes) == 0 {
		t.Fatal("parsed no `go test -tags` lanes from the PR-critical workflows. Either the tagged " +
			"lanes were removed, or this gate's YAML parsing broke -- both are failures")
	}

	var uncovered []string
	for _, f := range files {
		dir, name := f[0], f[1]
		if buildsUnder(t, dir, name, nil) {
			continue // the untagged `go test ./...` already runs it
		}
		rel, _ := filepath.Rel(root, filepath.Join(dir, name))
		rel = filepath.ToSlash(rel)
		relDir := filepath.ToSlash(filepath.Dir(rel))

		covered := false
		for _, l := range lanes {
			if buildsUnder(t, dir, name, l.tags) && l.covers(relDir) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		if reason, ok := excludedReason(t, dir, name); ok {
			t.Logf("%s -- deliberately not run: %s", rel, reason)
			continue
		}
		uncovered = append(uncovered, rel)
	}

	sort.Strings(uncovered)
	for _, rel := range uncovered {
		t.Errorf("%s does not compile in the default build and NO PR-critical CI lane runs it.\n\n"+
			"A //go:build-tagged test is invisible to the untagged `go test ./...`, so it asserts "+
			"nothing on any PR. Either add a `go test -tags <tag> ./%s/` step to "+
			".github/workflows/ci.yml, or add its tag to deliberatelyNotRunInCI with a reason that "+
			"would survive review (memql#2903).", rel, filepath.Dir(rel))
	}
}

// TestDeliberateExclusionsAreHonest keeps the exclusion list from rotting the
// other way: an entry whose files are gone, or whose reason is empty, reads as
// a considered decision and is not one.
func TestDeliberateExclusionsAreHonest(t *testing.T) {
	root := repoRoot(t)
	files := testFiles(t, root)

	tags := make([]string, 0, len(deliberatelyNotRunInCI))
	for tag := range deliberatelyNotRunInCI {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	for _, tag := range tags {
		if strings.TrimSpace(deliberatelyNotRunInCI[tag]) == "" {
			t.Errorf("deliberatelyNotRunInCI[%q] has an empty reason", tag)
		}
		found := false
		for _, f := range files {
			if !buildsUnder(t, f[0], f[1], nil) && buildsUnder(t, f[0], f[1], []string{tag}) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("deliberatelyNotRunInCI lists %q, but no tagged *_test.go requires it any more. "+
				"Remove the entry (memql#2903).", tag)
		}
	}
}
