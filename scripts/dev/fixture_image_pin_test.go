// Static guard over how ci.yml sources its Postgres service container
// (znasllc-io/memql#2793).
//
// Background: `mcp-conformance` and `db-tests` declare a `services:` container
// so the DB-gated suites have a real TimescaleDB. That image used to be pulled
// from Docker Hub, unauthenticated and unmirrored. A service container is
// pulled by the runner at JOB SETUP, before the first step executes, so a Hub
// blip killed the lane before a single test ran -- and because both lanes feed
// `ci-required`, the merge queue EVICTED the candidate. Measured twice in
// roughly nine hours (#2777 in mcp-conformance, #2826 in db-tests), and both
// were silent: nothing notifies you, `isInMergeQueue` flips back to false and
// the PR sits at CLEAN looking healthy.
//
// Two properties fix that, and BOTH have to hold:
//
//	availability -- pull from our GHCR mirror, not from Docker Hub
//	immutability -- pin by digest, so the fixture cannot move under a PR
//
// Either alone is insufficient. A mirrored floating tag still drifts silently
// (the second half of #2793); a Docker Hub digest still dies in a Hub outage.
//
// Both fail QUIETLY when they regress, which is why they get a test rather than
// a comment. Reverting to `timescale/timescaledb:latest-pg16` looks entirely
// reasonable in a diff and reintroduces an intermittent, merge-queue-eating
// failure that nobody attributes to the change that caused it.
package dev

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mirrorPrefix is the GHCR repository the fixture must come from. The mirror is
// published by .github/workflows/mirror-fixture-images.yml.
const mirrorPrefix = "ghcr.io/znasllc-io/"

// digestPinned matches `<repo>@sha256:<64 hex>` -- an immutable reference.
var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

type ciServices struct {
	Jobs map[string]struct {
		Permissions map[string]string `yaml:"permissions"`
		Services    map[string]struct {
			Image       string            `yaml:"image"`
			Credentials map[string]string `yaml:"credentials"`
		} `yaml:"services"`
	} `yaml:"jobs"`
	Permissions map[string]string `yaml:"permissions"`
}

func parseCI(t *testing.T) ciServices {
	t.Helper()
	var doc ciServices
	if err := yaml.Unmarshal([]byte(repoFile(t, ".github", "workflows", "ci.yml")), &doc); err != nil {
		t.Fatalf("parse .github/workflows/ci.yml: %v", err)
	}
	return doc
}

func ciServiceContainers(t *testing.T) map[string]string {
	t.Helper()
	images := map[string]string{}
	for jobName, job := range parseCI(t).Jobs {
		for svcName, svc := range job.Services {
			if svc.Image != "" {
				images[jobName+"."+svcName] = svc.Image
			}
		}
	}
	// Anti-vacuous floor: ci.yml has DB-gated lanes, so finding no service
	// containers means the parse or the schema drifted, not that the risk is
	// gone. Passing over an empty set is how this guard would silently retire.
	if len(images) == 0 {
		t.Fatal("no `services:` containers found in ci.yml; this guard cannot pass " +
			"vacuously -- if the DB lanes stopped using service containers, retarget " +
			"this test rather than deleting it (#2793)")
	}
	return images
}

// Every service container must come from the mirror and be pinned by digest.
func TestCIServiceImagesAreMirroredAndDigestPinned(t *testing.T) {
	for where, image := range ciServiceContainers(t) {
		if !strings.HasPrefix(image, mirrorPrefix) {
			t.Errorf("%s pulls its service container from %q. It must come from the "+
				"GHCR mirror (%s...): a service container is pulled at job setup, "+
				"before any step runs, so a registry outage kills the lane before a "+
				"test executes -- and both DB lanes feed ci-required, so the merge "+
				"queue silently evicts the candidate (#2793, #2777, #2826).",
				where, image, mirrorPrefix)
			continue
		}
		if !digestPinned.MatchString(image) {
			t.Errorf("%s pins its service container as %q. It must be pinned by "+
				"digest (@sha256:...): a floating tag lets the fixture change under a "+
				"PR with no diff, which is the silent-drift half of #2793. Refresh via "+
				"the mirror-fixture-images workflow and update the digest in a PR.",
				where, image)
		}
	}
}

// A GHCR package needs an authenticated pull, and the pull happens before any
// login step could run -- so the credentials must be declared inline on the
// service. Without them the runner attempts an anonymous pull and the lane dies
// at setup with a 401, which reads as an infrastructure flake.
func TestCIServiceImagesCarryRegistryCredentials(t *testing.T) {
	var checked int
	for jobName, job := range parseCI(t).Jobs {
		for svcName, svc := range job.Services {
			if !strings.HasPrefix(svc.Image, mirrorPrefix) {
				continue
			}
			checked++
			where := jobName + "." + svcName
			if len(svc.Credentials) == 0 {
				t.Errorf("%s pulls from the GHCR mirror but declares no `credentials:`. "+
					"The package is org-owned, and a service container is pulled at job "+
					"setup where no login step has run -- so the credentials must be "+
					"inline on the service (#2793)", where)
				continue
			}
			if pw := svc.Credentials["password"]; !strings.Contains(pw, "secrets.GITHUB_TOKEN") {
				t.Errorf("%s authenticates its mirror pull with %q; it must use the "+
					"automatic GITHUB_TOKEN. Needing a static secret is the dependency "+
					"the GHCR mirror was chosen to avoid (#2793)", where, pw)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no mirrored service containers found; this guard cannot pass vacuously (#2793)")
	}
}

// The token only carries package scope if the workflow asks for it. Without
// `packages: read` the credentials above authenticate as a token that cannot
// pull, and the failure lands at job setup looking like a registry problem.
//
// Checked as the EFFECTIVE permission of each job that owns a mirrored service,
// not merely at workflow scope. A job-level `permissions:` block REPLACES the
// workflow's rather than extending it -- GitHub's docs: "If you specify the
// access for any of these permissions, all of those that are not specified are
// set to none." So adding an ordinary least-privilege `permissions: {contents:
// read}` to db-tests silently strips `packages` from that job's token, the
// service pull 401s at job setup, and a workflow-scope-only assertion stays
// green. That is not hypothetical: the `changes` job in this same file already
// declares job-level permissions, so a tidy-up pass extending the pattern is
// exactly the reasonable-looking diff this guard exists to catch.
func TestCIRequestsPackagesReadPermission(t *testing.T) {
	doc := parseCI(t)

	// Workflow scope is the default every job inherits when it declares none.
	if got := doc.Permissions["packages"]; got == "write" {
		t.Error("ci.yml must not hold `packages: write` at workflow scope; it only " +
			"consumes the mirror. Publishing is mirror-fixture-images.yml's job (#2793)")
	}

	var checked int
	for jobName, job := range doc.Jobs {
		mirrored := false
		for _, svc := range job.Services {
			if strings.HasPrefix(svc.Image, mirrorPrefix) {
				mirrored = true
				break
			}
		}
		if !mirrored {
			continue
		}
		checked++

		// Effective scope: the job's own block if it declares one, else the
		// workflow's.
		effective, scope := doc.Permissions, "workflow"
		if len(job.Permissions) > 0 {
			effective, scope = job.Permissions, "job"
		}
		switch effective["packages"] {
		case "read", "write":
			if effective["packages"] == "write" && scope == "job" {
				t.Errorf("job %q holds `packages: write`; read is sufficient to pull "+
					"the mirror (#2793)", jobName)
			}
		default:
			t.Errorf("job %q pulls a mirrored service container, but its EFFECTIVE "+
				"permissions (%s scope: %v) do not grant `packages`. A job-level "+
				"`permissions:` block REPLACES the workflow's -- everything unlisted "+
				"becomes none -- so the GITHUB_TOKEN in its service credentials cannot "+
				"pull, and the lane 401s at job setup before any step runs, which reads "+
				"as a registry flake (#2793).", jobName, scope, effective)
		}
	}
	if checked == 0 {
		t.Fatal("no job owns a mirrored service container; this guard cannot pass " +
			"vacuously (#2793)")
	}
}
