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
//
// # The two remedies, and why they used to be visible one at a time
// (memql#3451)
//
// An orphan has exactly two correct fixes, and they are mutually exclusive:
// ROUTE it to a bucket some lane consumes, or EXEMPT it here. `.claude/
// settings.json` got one of each within an afternoon, each PR individually
// correct, each breaking `main` in the opposite direction -- because an author
// hitting this guard could see one remedy at a time and nothing named the
// condition that decides between them.
//
// It is one question: DOES A GATE READ THIS FILE? The failure message now
// answers it per orphan, from the tree, by listing which tests, workflows and
// Makefile targets name the path (see gate_corpus_test.go).
//
// The same evidence makes an exemption's REASON checkable, which is the third
// half of memql#3451 and the sharpest one. The entry that started it claimed
// "no lane consults it" about a file `scripts/dev/repo_hygiene_test.go` names
// outright. Prose asserting something false is worse than no prose: it is the
// part a future reader is entitled to trust. So every entry now declares the
// exact set of gate-corpus files that mention it, annotated one by one, and
// that declaration is compared against the tree in both directions.
package ci

import (
	"strings"
	"testing"
)

// exemption is one allow-list entry: why the path needs no lane, plus the
// machine-checkable form of that claim.
type exemption struct {
	// reason is the prose a future reader gets. Required.
	reason string

	// mentionedBy is the EXACT set of gate-corpus files that name any path
	// this entry exempts, each mapped to why that mention is not a gate
	// reading the file.
	//
	// Empty means "nothing in the corpus names it" -- the strongest form, and
	// the one the memql#3451 entry asserted falsely. Verified both ways: a new
	// mention fails until someone looks at it (usually the answer is to ROUTE
	// the file instead), and a mention that disappears fails too, because a
	// record kept for a reader has to stay true.
	mentionedBy map[string]string

	// coveredByWorkflow, when set, names a workflow OTHER than ci.yml that
	// exercises the file on EVERY pull request. Verified: the workflow exists,
	// triggers on `pull_request`, and carries no trigger-level path filter --
	// the three things that, if any changed, would silently make the reason
	// false.
	coveredByWorkflow string
}

// coverageAllowList names paths that legitimately reach no consumed bucket.
//
// Keys are patterns in the same grammar the buckets use, compiled by the same
// matcher, so an entry cannot mean something different here than it would in
// `ci.yml`.
var coverageAllowList = map[string]exemption{
	// --- git / build-context metadata, read by no gate ---
	".gitattributes": {
		reason: "git metadata; no gate reads it",
	},
	".dockerignore": {
		reason: "docker build-context metadata; no PR lane builds an image (see the Dockerfile note below)",
	},

	// --- GitHub-side configuration, interpreted by GitHub and not by a lane ---
	".github/CODEOWNERS": {
		reason: "GitHub review routing; interpreted by GitHub, read by no lane. It is not " +
			"in the mention corpus itself for the same reason: naming a path in CODEOWNERS " +
			"routes a REVIEWER, which tells you nothing about what CI runs",
	},

	// --- tool configs whose own workflow verifies them ---
	".gitleaks.toml": {
		reason: "verified by its own workflow rather than by a ci.yml bucket: gitleaks runs " +
			"on every PR with no path filter, and a malformed config fails that run. The CLI " +
			"DISCOVERS the file rather than being pointed at it, so the coverage claim is " +
			"expressed as the workflow rather than as a mention -- gitleaks.yml naming the " +
			"path in prose (below) does not change which lane reads it",
		coveredByWorkflow: "gitleaks.yml",
		mentionedBy: map[string]string{
			".github/workflows/gitleaks.yml": "prose, not a reference CI executes. The " +
				"workflow's header comment names this file to tell the next person that a " +
				"HISTORICAL finding is cleared by an entry here and never by editing the " +
				"fixture that introduced it -- the mistake three separate commits made " +
				"(memql#3484). The run step still passes no --config flag; the CLI discovers " +
				"the file exactly as before. Recorded rather than reworded away, per the " +
				"precedent set by the deploy/k8s/base/tls/*.sh entry: phrasing a comment to " +
				"dodge the check would be the first step to the check meaning nothing",
		},
	},
	".markdownlint.json": {
		reason: "editor/linter config; no CI lane runs markdownlint",
	},

	// --- static assets ---
	"LICENSE": {
		reason: "legal text; no gate reads it",
	},
	"assets/**": {
		reason: "branding images, not embedded (the embedded SVG set is component/mcp/*.svg, routed to gates)",
	},

	// --- placeholders ---
	"**/.gitkeep": {
		reason: "empty-directory placeholder; contains nothing to verify",
	},

	// --- shell outside scripts/ ---
	// The only .sh sweeps in the tree (scripts/lib/capability_contract_test.go)
	// walk `scripts/` and the four `scripts/{k3d,deploy,staging,release}`
	// directories. Nothing sweeps shell elsewhere, so these reach no gate.
	"infra/polyphon/*.sh": {
		reason: "shell outside scripts/; the only .sh gates (scripts/lib/capability_contract_test.go) are scoped to scripts/",
	},
	"deploy/k8s/base/tls/*.sh": {
		reason: "shell outside scripts/; same scope limit as infra/polyphon/*.sh. " +
			"scripts/k3d/seed-secrets.sh invokes gen-internal-ca.sh, but that is `make up` " +
			"on a developer's machine, not a lane -- which is why scripts/** is not in the " +
			"mention corpus",
		mentionedBy: map[string]string{
			"scripts/ci/gate_corpus_test.go": "the package comment cites this exact path as " +
				"its worked example of a runtime reference that is not a gate. Recorded here " +
				"rather than reworded away: phrasing a comment to dodge the check would be " +
				"the first step to the check meaning nothing",
		},
	},

	// --- image build inputs ---
	"docker/**": {
		reason: "the retired compose stack's build inputs; no lane builds them and no gate " +
			"reads them. Only docker/init-db.sql and docker/memql.Dockerfile are exempted " +
			"here -- docker/README.md is already routed by the `**/*.md` glob",
		mentionedBy: map[string]string{
			".github/workflows/ci.yml": "two comments on the db-tests service container note " +
				"that its `CREATE EXTENSION` lines mirror docker/init-db.sql. The lane types " +
				"the SQL out itself and never reads the file, so editing it cannot redden " +
				"anything -- the duplication is the point of the comment",
		},
	},
}

// coverageScan is one pass of the guard: which tracked files reach a consumed
// bucket, which do not, and which allow-list entry caught each survivor.
type coverageScan struct {
	files    []string
	consumed []string
	orphans  []string
	exempted map[string][]string // allow-list pattern -> the files it exempted
}

// scanCoverage classifies every tracked file. Shared by the guard and by the
// claim check so the two cannot disagree about what an entry is exempting.
func scanCoverage(t *testing.T) coverageScan {
	t.Helper()

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
		b, err := CompileBucket(pattern, []string{pattern})
		if err != nil {
			t.Fatalf("allow-list %v", err)
		}
		allow = append(allow, b)
	}

	scan := coverageScan{
		files:    trackedFiles(t),
		consumed: SortedKeys(consumed),
		exempted: map[string][]string{},
	}

	for _, f := range scan.files {
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
		// Every matching entry is recorded, not just the first. Breaking
		// early would let one entry shadow an overlapping one, which would
		// then be reported "stale" while it is merely redundant -- a
		// misleading failure on a guard whose whole job is precision.
		exempt := false
		for _, b := range allow {
			if b.Match(f) {
				scan.exempted[b.Name] = append(scan.exempted[b.Name], f)
				exempt = true
			}
		}
		if !exempt {
			scan.orphans = append(scan.orphans, f)
		}
	}
	return scan
}

// routingFork is the header every orphan failure carries. It names BOTH
// remedies and the single question that decides between them, because the
// memql#3451 breakage was two authors each seeing one of them.
const routingFork = "Two remedies, and ONE question decides between them: does a gate READ this file?\n" +
	"  (a) YES -> ROUTE it. Add the path to a bucket in the `changes` job that some lane's\n" +
	"      `if:` consults -- `gates` for anything a Go test / workflow / Makefile target\n" +
	"      reads, `go` for an input to a test the gate-inputs step does not run.\n" +
	"  (b) NO  -> EXEMPT it. Add a coverageAllowList entry with its reason AND the exact\n" +
	"      set of gate-corpus files that mention it (empty means nothing does). That\n" +
	"      declaration is verified against the tree, so the claim cannot be false the way\n" +
	"      it was in memql#3451.\n" +
	"Picking the wrong one breaks `main` for the next author, so the evidence is below."

// routingEvidence renders the per-orphan half: what the tree says, and which
// remedy that evidence supports.
func routingEvidence(orphan string, mentions []string) string {
	if len(mentions) > 0 {
		return "  " + orphan + "\n" +
			"      mentioned by: " + strings.Join(mentions, ", ") + "\n" +
			"      -> remedy (a), ROUTE it: something in the gate corpus names this path, so a\n" +
			"         change to it can redden a lane. Read those files; if any of them READS\n" +
			"         the file, route it to the bucket the lane running that test consults.\n" +
			"         Exempting it would record the claim memql#3451 was filed about."
	}
	return "  " + orphan + "\n" +
		"      mentioned by: nothing (no *_test.go, no .github/workflows/ file, not the Makefile)\n" +
		"      -> remedy (b), EXEMPT it: no gate corpus file names this path. Add a\n" +
		"         coverageAllowList entry with an empty mentionedBy. Note the residual, which\n" +
		"         the entry's reason should address: a test that sweeps `git ls-files` reads\n" +
		"         files without naming them, so absence of a mention is evidence, not proof."
}

// TestEveryTrackedFileReachesAConsumedBucket is the guard.
func TestEveryTrackedFileReachesAConsumedBucket(t *testing.T) {
	scan := scanCoverage(t)

	if len(scan.orphans) > 0 {
		corpus := loadGateCorpus(t)

		sample := scan.orphans
		if len(sample) > 25 {
			sample = sample[:25]
		}
		var evidence strings.Builder
		for _, o := range sample {
			evidence.WriteString(routingEvidence(o, corpus.mentionedBy(o)))
			evidence.WriteString("\n")
		}

		t.Errorf("%d of %d tracked files match NO consumed filter bucket.\n"+
			"A PR whose whole diff is one of these runs ZERO lanes and `ci-required` reports "+
			"green over a change nothing verified (the memql#2972 class, measured at 383, then "+
			"161, then 91).\n\n%s\n\nConsumed buckets: %v\nOrphans (first %d of %d):\n%s",
			len(scan.orphans), len(scan.files), routingFork,
			scan.consumed, len(sample), len(scan.orphans), evidence.String())
	}

	// An allow-list entry that stops matching is an exemption nobody removed.
	// Left alone it silently broadens on the next rename.
	for _, pattern := range SortedKeys(coverageAllowList) {
		if len(scan.exempted[pattern]) == 0 {
			t.Errorf("allow-list entry %q matches no orphan today. Either it was fixed (delete "+
				"the entry) or the path moved (update it) -- a stale exemption is an "+
				"unreviewed hole.", pattern)
		}
	}
}

// TestAllowListReasonsHoldAgainstTheTree is the memql#3451 half: an exemption
// asserts something about this repository, and the assertion is checked.
func TestAllowListReasonsHoldAgainstTheTree(t *testing.T) {
	scan := scanCoverage(t)
	corpus := loadGateCorpus(t)

	for _, pattern := range SortedKeys(coverageAllowList) {
		entry := coverageAllowList[pattern]

		if strings.TrimSpace(entry.reason) == "" {
			t.Errorf("allow-list entry %q carries no reason; every exemption must state why "+
				"the file needs no lane", pattern)
		}

		// The tree's answer, unioned over every file this entry exempts.
		actual := map[string]bool{}
		for _, f := range scan.exempted[pattern] {
			for _, m := range corpus.mentionedBy(f) {
				actual[m] = true
			}
		}

		for _, m := range SortedKeys(actual) {
			if _, declared := entry.mentionedBy[m]; !declared {
				t.Errorf("allow-list entry %q is exempted as needing no lane, but %s names a "+
					"path it covers, and the entry does not account for that.\n"+
					"This is memql#3451 exactly: the entry it was filed over claimed \"no lane "+
					"consults it\" about a file a test named outright.\n"+
					"Read %s. If it READS the file, delete this entry and ROUTE the path to a "+
					"bucket the lane running that test consults. If the mention is prose, or a "+
					"reference nothing in CI executes, record it in mentionedBy with the "+
					"sentence that says so.\nexempted files: %v",
					pattern, m, m, scan.exempted[pattern])
			}
		}
		for _, m := range SortedKeys(entry.mentionedBy) {
			if !actual[m] {
				t.Errorf("allow-list entry %q declares that %s mentions it, and nothing in that "+
					"file names any path the entry covers today.\n"+
					"Delete the row. A record kept for a future reader is worth exactly as much "+
					"as its accuracy, which is the whole argument of memql#3451.", pattern, m)
			}
			if strings.TrimSpace(entry.mentionedBy[m]) == "" {
				t.Errorf("allow-list entry %q records a mention by %s with no explanation. The "+
					"row exists to say why that mention is not a gate reading the file; without "+
					"the sentence it is an unexamined mention with extra steps.", pattern, m)
			}
		}

		if entry.coveredByWorkflow != "" {
			assertWorkflowRunsOnEveryPR(t, pattern, entry.coveredByWorkflow)
		}
	}
}

// assertWorkflowRunsOnEveryPR checks the `coveredByWorkflow` claim: the named
// workflow must exist, fire on pull requests, and narrow itself for nobody.
//
// A trigger-level `paths:` is the specific way this reason goes false in
// silence -- the workflow simply does not run for the PR that changed the file
// it was cited as covering.
func assertWorkflowRunsOnEveryPR(t *testing.T, pattern, workflow string) {
	t.Helper()

	wf := gateLoadWorkflow(t, workflow)
	if len(wf.On) == 0 {
		t.Fatalf("allow-list entry %q cites %s as its coverage, and that workflow parses to no "+
			"`on:` triggers at all", pattern, workflow)
	}
	spec, ok := wf.On["pull_request"]
	if !ok {
		t.Errorf("allow-list entry %q cites %s as covering it on every pull request, but that "+
			"workflow has no `pull_request` trigger. The exemption rests on a run that does "+
			"not happen (memql#3451).", pattern, workflow)
		return
	}
	mapping, ok := spec.(map[string]any)
	if !ok {
		return // `pull_request:` with no body cannot carry a path filter
	}
	for _, key := range []string{"paths", "paths-ignore"} {
		if _, found := mapping[key]; found {
			t.Errorf("allow-list entry %q cites %s as covering it on every pull request, but "+
				"that workflow now carries a trigger-level `%s` filter. A PR the filter "+
				"excludes runs neither the workflow nor -- because the path is exempt here -- "+
				"any ci.yml lane (memql#3451).", pattern, workflow, key)
		}
	}
}

// The fork message is the thing an author reads at 6pm, so its two branches
// are asserted rather than assumed. A guard that names only the remedy its
// author happened to take is how memql#3451 produced two correct, opposite
// fixes on one file in one afternoon.
func TestRoutingEvidenceNamesTheRemedyItsEvidenceSupports(t *testing.T) {
	for _, remedy := range []string{"ROUTE", "EXEMPT", "does a gate READ this file"} {
		if !strings.Contains(routingFork, remedy) {
			t.Errorf("the orphan failure header no longer names %q. Both remedies and the "+
				"condition that chooses between them have to appear, or an author sees one "+
				"of two mutually exclusive fixes (memql#3451).\ngot:\n%s", remedy, routingFork)
		}
	}

	mentioned := routingEvidence("some/file.txt", []string{"scripts/dev/repo_hygiene_test.go"})
	if !strings.Contains(mentioned, "ROUTE") || !strings.Contains(mentioned, "repo_hygiene_test.go") {
		t.Errorf("an orphan a gate corpus file names must be pointed at routing, with the "+
			"naming file quoted as the evidence.\ngot:\n%s", mentioned)
	}

	unmentioned := routingEvidence("some/file.txt", nil)
	if !strings.Contains(unmentioned, "EXEMPT") || !strings.Contains(unmentioned, "coverageAllowList") {
		t.Errorf("an orphan nothing names must be pointed at the allow list.\ngot:\n%s", unmentioned)
	}
	if strings.Contains(unmentioned, "mentioned by: nothing") == false {
		t.Errorf("the evidence line must say the corpus was consulted and found nothing, "+
			"rather than staying silent -- silence reads as \"not checked\".\ngot:\n%s", unmentioned)
	}
}
