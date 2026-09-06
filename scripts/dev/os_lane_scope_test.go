// Static guard over the OS lane's path bucket (znasllc-io/memql#3314, moved
// from the portal's bucket to the shell's by epic memql#4984).
//
// # The exact bug this exists to prevent, which has already happened once
//
// The `vscode` bucket originally listed only `editors/vscode/**` and
// `cmd/memql-lsp/**`. The extension consumes sdk/ts and sdk/ts-viewkit as
// `file:` DEPENDENCIES -- esbuild inlines them into out/extension.js -- so a
// change confined to either package could break the extension's typecheck,
// its tests and `vsce package`, while the sdkts / viewkit lanes ran that
// package's own tests, went green, and reported nothing. Renaming an export
// is enough to do it. The fix was to list the dependencies in the CONSUMER's
// bucket, and the comment above that bucket in ci.yml records the lesson.
//
// The OS shell has the identical shape: the same `file:` dependency, bundled
// by Vite instead of esbuild. So it starts with it listed -- and this guard
// is what keeps it listed, because dropping it looks like tidying a duplicate
// ("sdk/ts is already in the sdkts bucket") and restores the fail-open
// exactly.
//
// ONE DEPENDENCY, NOT TWO. The portal consumed sdk/ts-viewkit as well and the
// guard this replaces named both; the OS does not (clients/os/package.json),
// so listing it here would pin a pattern with no reason behind it -- and a
// pattern nobody can justify is the one somebody deletes for the wrong reason
// later.
//
// # Why on MEANING and not spelling
//
// Following gitleaks_scan_scope_test.go (#2996) and vscode_lane_scope_test.go
// (#2792): the workflow is PARSED, and the assertions read parsed values, so
// reformatting the bucket or the lane is fine and only a real narrowing
// fails. A pattern that appears in a comment is not a pattern.
package dev

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// osLaneJob is the job key this guard asserts on.
const osLaneJob = "os-checks"

// osFileDependencies are the workspace packages clients/os consumes via
// `file:`. Vite bundles them into the SPA, so a change to one can break the
// shell while its own lane stays green.
//
// Hardcoded rather than parsed out of clients/os/package.json ON PURPOSE.
// Deriving them would make the guard track whatever the package.json says,
// including "nothing" -- so removing a dependency from the manifest AND its
// bucket entry would pass. This is the one the repository has, and the list is
// short enough to state.
var osFileDependencies = []string{"sdk/ts/**"}

// changesBucketPatterns returns the compiled pattern list for one bucket,
// found via the `changes` job output that names the step supplying it -- the
// same indirection vscode_lane_scope_test.go uses, so a second paths-filter
// step cannot mask the real one.
func changesBucketPatterns(t *testing.T, bucket string) []string {
	t.Helper()

	var doc struct {
		Jobs map[string]struct {
			Outputs map[string]string `yaml:"outputs"`
			Steps   []struct {
				Id   string `yaml:"id"`
				With struct {
					Filters string `yaml:"filters"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(ciYAML(t), &doc); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}

	wiring := doc.Jobs["changes"].Outputs[bucket]
	m := regexp.MustCompile(`steps\.([A-Za-z0-9_-]+)\.outputs`).FindStringSubmatch(wiring)
	if m == nil {
		t.Fatalf("the `changes` job declares no %q output (got %q). The lane's `if:` reads "+
			"needs.changes.outputs.%s, so without the output the lane NEVER runs "+
			"(memql#3314)", bucket, wiring, bucket)
	}

	var filters string
	for _, s := range doc.Jobs["changes"].Steps {
		if s.Id == m[1] {
			filters = s.With.Filters
			break
		}
	}
	if filters == "" {
		t.Fatalf("step %q on the `changes` job declares no path filters; this guard cannot "+
			"pass vacuously", m[1])
	}

	var parsed map[string][]string
	if err := yaml.Unmarshal([]byte(filters), &parsed); err != nil {
		t.Fatalf("parse the `changes` filters block: %v", err)
	}
	patterns := parsed[bucket]
	if len(patterns) == 0 {
		t.Fatalf("the %q bucket declares no patterns; it would be false for every PR and "+
			"the lane would never run", bucket)
	}
	return patterns
}

// The bucket must cover the shell's tree itself.
func TestOsBucketCoversTheClientsTree(t *testing.T) {
	patterns := changesBucketPatterns(t, "osclient")
	for _, p := range patterns {
		// `clients/**` (the category) or `clients/os/**` (this inhabitant) both
		// satisfy it -- the guard owns the coverage, not the spelling.
		if p == "clients/**" || p == "clients/os/**" {
			return
		}
	}
	t.Errorf("the `osclient` bucket does not cover the shell's tree (got %v). A PR touching "+
		"only clients/os would path-skip os-checks, and no other lane runs the shell's "+
		"typecheck, tests or build -- so it would merge unverified.", patterns)
}

// The bucket must cover the shell's `file:` dependencies. THIS is the
// assertion that fails against the #2792 defect reproduced in a new bucket.
func TestOsBucketCoversItsFileDependencies(t *testing.T) {
	patterns := changesBucketPatterns(t, "osclient")
	for _, dep := range osFileDependencies {
		found := false
		for _, p := range patterns {
			if p == dep {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the `osclient` bucket no longer lists %q (got %v).\n"+
				"clients/os consumes it as a `file:` dependency and Vite bundles it "+
				"into the SPA, so a change confined to that package can break the shell's "+
				"typecheck, its tests and `vite build` -- while that package's OWN lane "+
				"runs its own tests, goes green, and reports nothing. This is memql#2792 "+
				"verbatim, in a different bucket. It is in this bucket as well as its own "+
				"deliberately; that is not a duplicate to tidy.", dep, patterns)
		}
	}
}

// A lane that is not selected verifies nothing. Reads the condition without
// owning its shape: any `if:` that still consults the bucket passes.
func TestOsLaneRunsOnShellOnlyChanges(t *testing.T) {
	doc := parseCIJobs(t)
	job, ok := doc.Jobs[osLaneJob]
	if !ok {
		t.Fatalf("no %q job in .github/workflows/ci.yml; if the lane was renamed, retarget "+
			"this guard rather than deleting it (memql#3314)", osLaneJob)
	}
	cond := strings.TrimSpace(job.If)
	if cond == "" {
		return // unconditional: stronger still
	}
	if !strings.Contains(cond, "needs.changes.outputs.osclient") {
		t.Errorf("the %q job's `if:` no longer consults the `osclient` bucket, so a "+
			"shell-only PR would not select this lane -- and no other lane runs the "+
			"shell's checks.\ngot if: %s", osLaneJob, cond)
	}
}

// The lane must actually run the checks. A lane that installs and stops is a
// lane that proves the dependency tree resolves and nothing else.
func TestOsLaneRunsTypecheckTestAndBuild(t *testing.T) {
	doc := parseCIJobs(t)
	job := doc.Jobs[osLaneJob]
	if len(job.Steps) == 0 {
		t.Fatalf("%q declares no steps; this guard cannot pass vacuously", osLaneJob)
	}

	var commands []string
	for _, s := range job.Steps {
		if s.ContinueOnError != nil && s.ContinueOnError != false {
			t.Errorf("step %q sets continue-on-error=%v; its failure would not fail the "+
				"lane", s.Name, s.ContinueOnError)
		}
		commands = append(commands, commandLines(s.Run)...)
	}
	joined := strings.Join(commands, "\n")

	// The three make targets that constitute "the shell is verified". Named as
	// targets rather than as raw npm invocations because the targets are the
	// contract -- scripts/os/build.sh is free to change how they work.
	for _, target := range []string{"os-typecheck", "os-test", "os-build"} {
		if !strings.Contains(joined, target) {
			t.Errorf("the %q lane never runs `make %s`. Every one of the three catches a "+
				"different class: typecheck catches types, test catches behaviour, and "+
				"build catches what neither sees -- an unresolvable Tailwind token, a "+
				"broken CSS import, a dynamic import that does not exist. The image build "+
				"runs the build step, so a lane without it ships a bundle CI never "+
				"produced.\ncommands: %s", osLaneJob, target, joined)
		}
	}
}
