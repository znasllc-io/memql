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
	"fmt"
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
	// Keys are `any` so a parser that reads a bare `on` as the YAML 1.1 boolean
	// true is handled too. yaml.v3 follows YAML 1.2 and yields the string "on",
	// which is the path actually taken here; the boolean arm is belt-and-braces
	// for a parser swap, not a case this repo hits today.
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

	// `on:` has three legal shapes and all three can express manual-only:
	// a mapping, a list, or a bare string. Normalise to a name set rather than
	// rejecting the forms this file does not happen to use -- a guard that
	// fatals on a legitimate rewrite is noise, not safety.
	names := map[string]bool{}
	switch v := raw.(type) {
	case map[string]any:
		for name := range v {
			names[name] = true
		}
	case []any:
		for _, item := range v {
			names[fmt.Sprint(item)] = true
		}
	case string:
		names[v] = true
	default:
		t.Fatalf("unexpected `on:` shape %T; expected a mapping, list or string (#2793)", raw)
	}
	if len(names) == 0 {
		t.Fatal("mirror workflow declares no triggers; this guard cannot pass vacuously (#2793)")
	}
	for name := range names {
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

// The mirror must copy the manifest registry-to-registry, not pull/tag/push.
//
// This is a regression guard over a bug that was in this workflow and was caught
// before merge. `docker pull` + `docker tag` + `docker push` is wrong twice:
//
//   - it FLATTENS a multi-arch source. `timescale/timescaledb:latest-pg16` is an
//     OCI image index (386 / amd64 / arm64 confirmed via `docker manifest
//     inspect`); a pull resolves one platform, so the mirror silently loses the
//     rest.
//   - `{{index .RepoDigests 0}}` on the tagged image returns the SOURCE repo's
//     digest, not the mirror's. Measured locally: after tagging alpine into a
//     second repo, RepoDigests is
//     ["alpine@sha256:d9e853...", "ghcr.io/.../probe@sha256:d9e853..."] and
//     index 0 is the source. Since the source is an index and a pushed
//     single-platform image is not, the two digests genuinely differ -- so the
//     run summary would advertise a digest that does not exist in the mirror,
//     and whoever pinned it in ci.yml would get an unpullable image.
//
// The failure mode is the reason this is a test rather than a comment: both
// halves are silent. The workflow would report success.
func TestMirrorWorkflowCopiesTheManifestRatherThanRepushing(t *testing.T) {
	wf := mirrorWorkflow(t)
	var cmds []string
	for _, line := range strings.Split(wf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // the rationale above quotes the wrong form on purpose
		}
		cmds = append(cmds, trimmed)
	}
	body := strings.Join(cmds, "\n")

	if !strings.Contains(body, "imagetools create") {
		t.Error("the mirror must use `docker buildx imagetools create`, which copies " +
			"the manifest registry-to-registry and preserves a multi-arch index (#2793)")
	}
	for _, banned := range []string{"docker push", "docker tag", "RepoDigests"} {
		if strings.Contains(body, banned) {
			t.Errorf("the mirror must not use %q: pull/tag/push flattens a multi-arch "+
				"source, and RepoDigests[0] is the SOURCE repo's digest, so the "+
				"published digest would be misreported. Both fail silently (#2793).", banned)
		}
	}
	// The digest must be read back from the TARGET, so what is advertised is
	// what was published rather than what we believe was published.
	if !strings.Contains(body, "imagetools inspect") {
		t.Error("the mirror must read its digest back with `imagetools inspect` on " +
			"the target, rather than inferring it locally (#2793)")
	}
}

// The mirror must push to GHCR and authenticate with the automatic token.
// Switching it to a registry needing a static secret reintroduces the
// credential dependency this approach was chosen to avoid (#2793): a
// `services:` container is pulled before any step runs, so it can only ever use
// a credential that already exists for the job.
// Asserts on the PARSED login `with.registry` and the step's `env.TARGET`, not
// on the file text. An earlier version did `strings.Contains` over the whole
// file and was satisfied by the word `ghcr.io` appearing in an input
// `description:` and in a rationale comment -- so swapping the workflow wholesale
// to ACR left it green. That is the same "right check, wrong surface" defect
// caught twice in the #2792 guard; the file text is not the configuration.
func TestMirrorWorkflowTargetsGHCRWithTheAutomaticToken(t *testing.T) {
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				With map[string]string `yaml:"with"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(mirrorWorkflow(t)), &doc); err != nil {
		t.Fatalf("parse mirror-fixture-images.yml: %v", err)
	}

	var sawRegistry, sawTarget bool
	for _, job := range doc.Jobs {
		for _, step := range job.Steps {
			if reg, ok := step.With["registry"]; ok {
				sawRegistry = true
				if reg != "ghcr.io" {
					t.Errorf("the mirror logs in to %q; it must be ghcr.io. GHCR with "+
						"the automatic token is the only registry a `services:` pull can "+
						"authenticate against without a new secret, because that pull "+
						"happens at job setup before any login step runs (#2793)", reg)
				}
				if pw := step.With["password"]; !strings.Contains(pw, "secrets.GITHUB_TOKEN") {
					t.Errorf("the mirror authenticates with %q; it must use the automatic "+
						"GITHUB_TOKEN. A static secret is the dependency this approach was "+
						"chosen to avoid (#2793)", pw)
				}
			}
			if tgt, ok := step.Env["TARGET"]; ok {
				sawTarget = true
				if !strings.HasPrefix(tgt, "ghcr.io/") {
					t.Errorf("the mirror pushes to %q; the target must be under ghcr.io/ "+
						"(#2793)", tgt)
				}
			}
		}
	}
	if !sawRegistry {
		t.Fatal("no login step with a `registry:` found; this guard cannot pass vacuously (#2793)")
	}
	if !sawTarget {
		t.Fatal("no step declaring `env.TARGET` found; this guard cannot pass vacuously (#2793)")
	}
}
