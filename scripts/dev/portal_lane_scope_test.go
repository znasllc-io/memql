// Static guard over the portal lane's path bucket (znasllc-io/memql#3314).
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
// The portal has the identical shape: the same two `file:` dependencies,
// bundled by Vite instead of esbuild. So it starts with them listed -- and
// this guard is what keeps them listed, because dropping one looks like
// tidying a duplicate ("sdk/ts is already in the sdkts bucket") and restores
// the fail-open exactly.
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

// portalLaneJob is the job key this guard asserts on.
const portalLaneJob = "portal-checks"

// portalFileDependencies are the workspace packages clients/portal consumes
// via `file:`. Vite bundles them into the SPA, so a change to either can
// break the portal while their own lanes stay green.
//
// Hardcoded rather than parsed out of clients/portal/package.json ON PURPOSE.
// Deriving them would make the guard track whatever the package.json says,
// including "nothing" -- so removing a dependency from the manifest AND its
// bucket entry would pass. These are the two the repository has, and the list
// is short enough to state.
var portalFileDependencies = []string{"sdk/ts/**", "sdk/ts-viewkit/**"}

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

// The bucket must cover the portal tree itself.
func TestPortalBucketCoversTheClientsTree(t *testing.T) {
	patterns := changesBucketPatterns(t, "portal")
	for _, p := range patterns {
		// `clients/**` (the category) or `clients/portal/**` (this inhabitant)
		// both satisfy it -- the guard owns the coverage, not the spelling.
		if p == "clients/**" || p == "clients/portal/**" {
			return
		}
	}
	t.Errorf("the `portal` bucket does not cover the portal tree (got %v). A PR touching "+
		"only clients/portal would path-skip portal-checks, and no other lane runs the "+
		"portal's typecheck, tests or build -- so it would merge unverified.", patterns)
}

// The bucket must cover the portal's `file:` dependencies. THIS is the
// assertion that fails against the #2792 defect reproduced in a new bucket.
func TestPortalBucketCoversItsFileDependencies(t *testing.T) {
	patterns := changesBucketPatterns(t, "portal")
	for _, dep := range portalFileDependencies {
		found := false
		for _, p := range patterns {
			if p == dep {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the `portal` bucket no longer lists %q (got %v).\n"+
				"clients/portal consumes it as a `file:` dependency and Vite bundles it "+
				"into the SPA, so a change confined to that package can break the portal's "+
				"typecheck, its tests and `vite build` -- while that package's OWN lane "+
				"runs its own tests, goes green, and reports nothing. This is memql#2792 "+
				"verbatim, in a different bucket. It is in this bucket as well as its own "+
				"deliberately; that is not a duplicate to tidy.", dep, patterns)
		}
	}
}

// A lane that is not selected verifies nothing. Reads the condition without
// owning its shape: any `if:` that still consults the bucket passes.
func TestPortalLaneRunsOnPortalOnlyChanges(t *testing.T) {
	doc := parseCIJobs(t)
	job, ok := doc.Jobs[portalLaneJob]
	if !ok {
		t.Fatalf("no %q job in .github/workflows/ci.yml; if the lane was renamed, retarget "+
			"this guard rather than deleting it (memql#3314)", portalLaneJob)
	}
	cond := strings.TrimSpace(job.If)
	if cond == "" {
		return // unconditional: stronger still
	}
	if !strings.Contains(cond, "needs.changes.outputs.portal") {
		t.Errorf("the %q job's `if:` no longer consults the `portal` bucket, so a "+
			"portal-only PR would not select this lane -- and no other lane runs the "+
			"portal's checks.\ngot if: %s", portalLaneJob, cond)
	}
}

// The lane must actually run the checks. A lane that installs and stops is a
// lane that proves the dependency tree resolves and nothing else.
func TestPortalLaneRunsTypecheckTestAndBuild(t *testing.T) {
	doc := parseCIJobs(t)
	job := doc.Jobs[portalLaneJob]
	if len(job.Steps) == 0 {
		t.Fatalf("%q declares no steps; this guard cannot pass vacuously", portalLaneJob)
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

	// The three make targets that constitute "the portal is verified". Named
	// as targets rather than as raw npm invocations because the targets are
	// the contract -- scripts/portal/build.sh is free to change how they work.
	for _, target := range []string{"portal-typecheck", "portal-test", "portal-build"} {
		if !strings.Contains(joined, target) {
			t.Errorf("the %q lane never runs `make %s`. Every one of the three catches a "+
				"different class: typecheck catches types, test catches behaviour, and "+
				"build catches what neither sees -- an unresolvable Tailwind token, a "+
				"broken CSS import, a dynamic import that does not exist. The image build "+
				"runs the build step, so a lane without it ships a bundle CI never "+
				"produced.\ncommands: %s", portalLaneJob, target, joined)
		}
	}
}
