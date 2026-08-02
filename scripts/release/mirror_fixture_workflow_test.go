// Static guard over the CI test-fixture mirror workflow
// (.github/workflows/mirror-fixture-images.yml), memql#2793.
//
// That workflow copies the third-party Postgres image `ci.yml` runs as a
// `services:` container into this org's GHCR, so the DB-gated lanes stop
// pulling it from Docker Hub at job-setup time. Two merge-queue candidates were
// evicted by Docker Hub timeouts in roughly nine hours (#2777, #2826), both
// silently -- nothing notifies you when the queue drops a PR.
//
// These assertions pin the properties that make the mirror SAFE rather than the
// ones that make it work. A broken workflow fails loudly on its next dispatch;
// the invariants below fail QUIETLY, by letting the fixture move under a PR or
// by widening a write token, so they get a test.
//
// The real push is exercised by a workflow_dispatch on main, exactly as
// build_workflows_test.go says of the engine build next door.
package release

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mirrorWorkflowPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/release/ -> repo root is two directories up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", ".github", "workflows", "mirror-fixture-images.yml")
}

func mirrorWorkflow(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(mirrorWorkflowPath(t))
	if err != nil {
		t.Fatalf("read mirror-fixture-images.yml: %v", err)
	}
	return string(raw)
}

// The fixture must not move on its own. A `push:` or `schedule:` trigger would
// publish a new digest without anyone updating the pin in ci.yml -- and the
// moment someone later swaps the pin for a floating tag, that becomes the
// silent-drift half of #2793 rebuilt inside the fix for it.
func TestMirrorWorkflowIsManualOnly(t *testing.T) {
	// Keys are `any`: YAML 1.1 reads a bare `on` as the boolean true, and which
	// spelling a parser produces is not something this guard should depend on.
	var doc map[any]any
	if err := yaml.Unmarshal([]byte(mirrorWorkflow(t)), &doc); err != nil {
		t.Fatalf("parse mirror-fixture-images.yml: %v", err)
	}
	raw, ok := doc["on"]
	if !ok {
		raw, ok = doc[true]
	}
	if !ok {
		t.Fatal("mirror workflow declares no triggers; this guard cannot pass vacuously (#2793)")
	}
	triggers, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("unexpected `on:` shape %T; expected a mapping of trigger names (#2793)", raw)
	}
	if len(triggers) == 0 {
		t.Fatal("mirror workflow declares no triggers; this guard cannot pass vacuously (#2793)")
	}
	for name := range triggers {
		if name != "workflow_dispatch" {
			t.Errorf("mirror workflow must be workflow_dispatch-only; found trigger %q. "+
				"An automatic push publishes a new digest that nothing references, and "+
				"invites replacing ci.yml's digest pin with a floating tag -- which is "+
				"the silent-drift half of #2793 rebuilt inside its own fix.", name)
		}
	}
}

// `packages: write` is a token that can publish under the org. It belongs on
// the one job that pushes, never at workflow scope where every future job
// inherits it.
func TestMirrorWorkflowScopesPackagesWriteToTheJob(t *testing.T) {
	var doc struct {
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(mirrorWorkflow(t)), &doc); err != nil {
		t.Fatalf("parse mirror-fixture-images.yml: %v", err)
	}
	if doc.Permissions["packages"] == "write" {
		t.Error("`packages: write` must not sit at workflow scope; scope it to the " +
			"job that pushes, so a future job added to this file does not inherit a " +
			"registry-write token (#2793)")
	}
	if len(doc.Jobs) == 0 {
		t.Fatal("mirror workflow declares no jobs; this guard cannot pass vacuously (#2793)")
	}
	var writers int
	for name, job := range doc.Jobs {
		if job.Permissions["packages"] == "write" {
			writers++
		}
		if job.Permissions["contents"] != "read" {
			t.Errorf("job %q must declare `contents: read`; least privilege is the "+
				"convention every workflow in this repo follows (#2793)", name)
		}
	}
	if writers != 1 {
		t.Errorf("expected exactly one job holding `packages: write`, found %d. The "+
			"mirror pushes from a single job by design (#2793)", writers)
	}
}

// Every `uses:` in this repo is pinned to a 40-hex commit SHA rather than a
// moving tag. A mutable tag on an action that holds a registry-write token is
// the supply-chain shape worth being strict about.
var sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

func TestMirrorWorkflowPinsActionsBySHA(t *testing.T) {
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(mirrorWorkflow(t)), &doc); err != nil {
		t.Fatalf("parse mirror-fixture-images.yml: %v", err)
	}
	var checked int
	for _, job := range doc.Jobs {
		for _, step := range job.Steps {
			if step.Uses == "" {
				continue
			}
			checked++
			_, ref, found := strings.Cut(step.Uses, "@")
			if !found || !sha40.MatchString(ref) {
				t.Errorf("action %q must be pinned to a 40-hex commit SHA, not a "+
					"mutable tag: this workflow's job holds `packages: write` (#2793)", step.Uses)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no `uses:` steps found in the mirror workflow; this guard cannot " +
			"pass vacuously (#2793)")
	}
}

// The mirror must push to GHCR and authenticate with the automatic token.
// Switching it to a registry needing a static secret reintroduces the
// credential dependency this approach was chosen to avoid (#2793): a
// `services:` container is pulled before any step runs, so it can only ever use
// a credential that already exists for the job.
func TestMirrorWorkflowTargetsGHCRWithTheAutomaticToken(t *testing.T) {
	wf := mirrorWorkflow(t)
	for _, want := range []string{"ghcr.io", "secrets.GITHUB_TOKEN"} {
		if !strings.Contains(wf, want) {
			t.Errorf("mirror workflow must reference %q: GHCR with the automatic "+
				"token is the only registry a `services:` pull can authenticate "+
				"against without a new secret (#2793)", want)
		}
	}
}
