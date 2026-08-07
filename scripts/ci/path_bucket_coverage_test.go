// Static guard: every tracked file reaches a filter bucket some lane actually
// consumes (znasllc-io/memql#3223).
//
// # The direction that was never guarded
//
// This repository already asserts pattern -> path: `TestEveryFilterBucketPathExists`
// checks that each declared pattern names something real, and
// `TestGatesBucketCoversEveryKnownGateInput` checks that each known gate input
// is routed. Both walk from the CONFIG outwards.
//
// Neither can see an orphan, because an orphan is defined by the ABSENCE of a
// pattern. Walking from the config, there is nothing to walk from.
//
// So a file could be added that matched no consumed bucket, every lane's `if:`
// evaluated false, zero lanes ran, and `ci-required` reported green over a
// diff nothing verified. This is not theoretical: memql#2972 measured the class
// at 383 files and closed it; it regrew to 161; the routing that landed with
// memql#3165's prep took it to 91. Three measurements, no test looking in this
// direction, so it regrew twice.
//
// # "Consumed" is the load-bearing word
//
// A bucket declared but wired to no lane's `if:` routes NOTHING, so it cannot
// count as coverage. The target-module buckets from memql#3163 (base, engine,
// platform, integrations, server-*, app) are deliberately in that state until
// the module split, and `integrations/**` alone would otherwise "cover" 381
// files that in truth reach no lane at all. Counting a declared-but-unwired
// bucket as coverage is how a file with no CI looks verified -- the same
// mistake in a different costume.
//
// # Why yaml/yml/.env route to `gates`
//
// Not by taste. `cmd/envscan/scan.repoCorpus` walks the whole repository and
// reads every `.go`, `.memql`, `.yaml`, `.yml` and `.env*` file, and
// `TestNoEnvRegistryDrift` fails when a registry entry appears nowhere in that
// corpus. `./cmd/envscan/...` runs in the gate-inputs step, which fires on
// `gates`.
//
// So deleting the last reference to a registered env var from, say,
// `deploy/k8s/base/agent.yaml` turns `main` red -- and before this change that
// PR matched no consumed bucket, ran zero lanes, merged green, and handed the
// failure to the next unrelated author. `.go` and `.memql` in that corpus were
// already routed by `go` and `gates`; the yaml/env half was not.
package ci

import (
	"testing"
)

// coverageAllowList names paths that legitimately reach no consumed bucket.
//
// Keys are patterns in the same grammar the buckets use, compiled by the same
// matcher, so an entry cannot mean something different here than it would in
// `ci.yml`. Every entry carries the reason it is exempt; an entry without a
// verified reason is just an unguarded file with extra steps.
var coverageAllowList = map[string]string{
	// --- git / build-context metadata, read by no lane ---
	".gitignore":     "git metadata; no gate reads it",
	".gitattributes": "git metadata; no gate reads it",
	".dockerignore":  "docker build-context metadata; no PR lane builds an image (see the Dockerfile entries below)",

	// --- GitHub-side configuration, interpreted by GitHub and not by a lane ---
	".github/CODEOWNERS": "GitHub review routing; interpreted by GitHub, read by no lane",

	// --- tool configs whose own workflow verifies them ---
	".gitleaks.toml": "consumed by .github/workflows/gitleaks.yml, which runs on every PR with no " +
		"path filter, so it is verified by its own workflow rather than by a ci.yml bucket",
	".markdownlint.json": "editor/linter config; verified no CI lane runs markdownlint " +
		"(no reference in .github/, Makefile or scripts/)",

	// --- static assets ---
	"LICENSE":   "legal text; no gate reads it",
	"assets/**": "branding images, not embedded (the embedded SVG set is component/mcp/*.svg, routed to gates)",

	// --- placeholders ---
	"**/.gitkeep": "empty-directory placeholder; contains nothing to verify",

	// --- shell outside scripts/ ---
	// The only .sh sweeps in the tree (scripts/lib/capability_contract_test.go)
	// walk `scripts/` and the four `scripts/{k3d,deploy,staging,release}`
	// directories. Nothing sweeps shell elsewhere, so these reach no gate.
	"infra/polyphon/*.sh":      "shell outside scripts/; the only .sh gates (scripts/lib/capability_contract_test.go) are scoped to scripts/",
	"deploy/k8s/base/tls/*.sh": "shell outside scripts/; same scope limit as infra/polyphon/*.sh",

	// --- image build inputs ---
	// Stated as a KNOWN GAP rather than as "needs no CI". These are built by
	// build-engine-images.yml / deploy-gate-image.yml, both `workflow_dispatch`
	// on main, so no pull-request lane builds them and no ci.yml bucket can
	// cover them. Closing that is a separate change to those workflows.
	"Dockerfile":                       "image build input; built only by workflow_dispatch image workflows, never by a PR lane (known gap)",
	"docker/**":                        "image build inputs; built only by workflow_dispatch image workflows, never by a PR lane (known gap)",
	"cmd/deploy-gate-check/Dockerfile": "image build input; built only by deploy-gate-image.yml on workflow_dispatch (known gap)",
}

// TestEveryTrackedFileReachesAConsumedBucket is the guard.
func TestEveryTrackedFileReachesAConsumedBucket(t *testing.T) {
	raw, err := ReadCIWorkflow()
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	buckets, err := ParseChangesFilters(raw)
	if err != nil {
		t.Fatal(err)
	}
	consumed := ConsumedBuckets(raw)

	var active []*BucketMatcher
	for _, name := range SortedKeys(buckets) {
		if !consumed[name] {
			continue
		}
		b, err := CompileBucket(name, buckets[name])
		if err != nil {
			t.Fatalf("%v", err)
		}
		active = append(active, b)
	}
	if len(active) == 0 {
		t.Fatal("no declared bucket is consumed by any lane -- the guard fails closed rather " +
			"than declaring every file an orphan")
	}

	allow := make([]*BucketMatcher, 0, len(coverageAllowList))
	for _, pattern := range SortedKeys(coverageAllowList) {
		if coverageAllowList[pattern] == "" {
			t.Errorf("allow-list entry %q carries no reason; every exemption must state why the "+
				"file needs no lane", pattern)
			continue
		}
		b, err := CompileBucket("allow:"+pattern, []string{pattern})
		if err != nil {
			t.Fatalf("allow-list %v", err)
		}
		allow = append(allow, b)
	}

	files := trackedFiles(t)

	var orphans []string
	usedAllow := map[string]bool{}
	for _, f := range files {
		covered := false
		for _, b := range active {
			if b.Match(f) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		exempt := false
		for _, b := range allow {
			if b.Match(f) {
				usedAllow[b.Name] = true
				exempt = true
				break
			}
		}
		if !exempt {
			orphans = append(orphans, f)
		}
	}

	if len(orphans) > 0 {
		sample := orphans
		if len(sample) > 25 {
			sample = sample[:25]
		}
		t.Errorf("%d of %d tracked files match NO consumed filter bucket.\n"+
			"A PR whose whole diff is one of these runs ZERO lanes and `ci-required` reports "+
			"green over a change nothing verified (the memql#2972 class, measured at 383, then "+
			"161, then 91).\n"+
			"Route each to a bucket a lane consumes, or add it to coverageAllowList with a "+
			"stated reason.\nConsumed buckets: %v\nOrphans (first %d):\n  %v",
			len(orphans), len(files), SortedKeys(consumed), len(sample), sample)
	}

	// An allow-list entry that stops matching is an exemption nobody removed.
	// Left alone it silently broadens on the next rename.
	for _, pattern := range SortedKeys(coverageAllowList) {
		if !usedAllow["allow:"+pattern] {
			t.Errorf("allow-list entry %q matches no orphan today. Either it was fixed (delete "+
				"the entry) or the path moved (update it) -- a stale exemption is an "+
				"unreviewed hole.", pattern)
		}
	}
}
