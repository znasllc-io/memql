package main

import (
	"os"
	"os/exec"
	"path"
	"strings"
	"testing"
)

// TestDocsFrontMatterConformsToStandard and TestDocsRootLayoutIsClosed
// enforce DOCS_STANDARD.md §1-§2 (memql#4087, the repo-cleanup-docs-update
// campaign's Task 1) -- the two structural properties every later task in
// the campaign depends on staying true: every doc under docs/ carries the
// six-key front-matter block the site pipeline reads, and the docs/ ROOT
// itself holds nothing but the standard and its own index.
//
// # Why a gate and not just a written standard
//
// DOCS_STANDARD.md §2 has documented the six-key block since the standard
// was written (epic znasllc-io/memql#1167), but nothing enforced it. The
// probe that motivated this gate found roughly 20 files under docs/ that
// were missing the block entirely or missing at least one key, PLUS a much
// larger, silent drift the presence-only probe never surfaces: closed-set
// VALUES that were never valid. Every file under docs/internal/{design,
// planning,ops,program}/ that predates this gate used `area: internal` --
// the top-level directory name -- rather than the sub-bucket
// (`design`/`planning`/`ops`) §2 actually defines; a further ~14 files
// carried ADR-era status vocabulary (`accepted`, `design`, `current`,
// `proposed`) that was never in §2's `stable|draft|historical` set. A
// missing key is visible on a `head -1` probe; a wrong-but-present value
// is not, and is exactly the shape of the site pipeline's "silently drops
// this file" failure mode: `audience: public` is stringmatched by the site
// build, so a typo'd or unrecognised value there does not error, it just
// omits the page.
//
// # What this deliberately does NOT catch
//
//   - Front-matter VALUES for keys outside the closed sets checked here
//     (title text, owner handle validity, sinceVersion's format beyond
//     "non-empty"). A truthful-but-unverifiable claim is a content review
//     problem, not a structural one.
//   - Anchor / cross-reference validity inside the front-matter block
//     itself (e.g. `owner: someone-who-left`). Out of scope for a
//     structural gate; see docs_relative_links_test.go for the (also
//     structural, not content) in-repo-link gate.
//   - docs/superpowers/** (specs and plans quote banned/pre-standard forms
//     by design) and docs/public/reference/_generated/** (machine-generated
//     at release time; front-matter there is a generator concern, not an
//     authoring one).
//
// FALSE-POSITIVE ESCAPE HATCH: a genuinely new area of documentation that
// does not fit the closed `area` set, or a doc whose lifecycle does not fit
// `stable|draft|historical` (the `status` set), should widen DOCS_STANDARD.md
// §2's sets and this test's closed-set constants IN THE SAME CHANGE -- the
// standard and the gate must always state the same rule. Do not special-case
// an individual file path here; that reintroduces the "known sites only"
// pattern that the lifecycle-docs gate's review history (see
// lifecycle_docs_conformance_test.go) found misses the next paraphrase just
// as easily as the next value.
var (
	frontMatterRequiredKeys = []string{"title", "audience", "status", "area", "sinceVersion", "owner"}

	frontMatterClosedSets = map[string][]string{
		"audience": {"public", "internal", "ops"},
		"status":   {"stable", "draft", "historical"},
		"area": {
			"overview", "concepts", "language", "ai", "operate", "build",
			"cockpit", "design", "planning", "ops",
		},
	}
)

// docsFrontMatterExempt reports whether rel (a git-tracked path) is exempt
// from the front-matter gate: docs/superpowers/** by design, and the
// generated reference tree because nothing there is hand-authored.
func docsFrontMatterExempt(rel string) bool {
	return strings.HasPrefix(rel, "docs/superpowers/") ||
		strings.HasPrefix(rel, "docs/public/reference/_generated/")
}

// gitTrackedDocsFiles enumerates tracked docs/**.md paths via `git ls-files
// -z`, per the house convention (lifecycle_docs_conformance_test.go): a
// filepath.WalkDir sweep would see build artifacts and untracked scratch
// files as if they were repo content.
func gitTrackedDocsFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z", "docs").Output()
	if err != nil {
		t.Fatalf("git ls-files -z docs: %v", err)
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || path.Ext(rel) != ".md" {
			continue
		}
		if docsFrontMatterExempt(rel) {
			continue
		}
		files = append(files, rel)
	}
	return files
}

func TestDocsFrontMatterConformsToStandard(t *testing.T) {
	files := gitTrackedDocsFiles(t)
	if len(files) == 0 {
		t.Fatal("git ls-files -z docs returned no .md files -- this gate would verify nothing. " +
			"Either docs/ lost every markdown file, or gitTrackedDocsFiles stopped recognising them.")
	}

	var failures int
	for _, rel := range files {
		data, err := os.ReadFile(rel)
		if err != nil {
			// git-tracked but locally absent (partial checkout): not repo drift.
			continue
		}
		block, ok := parseFrontMatterBlock(string(data))
		if !ok {
			t.Errorf("%s: does not start with a `---` front-matter block (DOCS_STANDARD.md §2)", rel)
			failures++
			continue
		}

		for _, key := range frontMatterRequiredKeys {
			val, present := block[key]
			if !present {
				t.Errorf("%s: front-matter is missing required key %q (DOCS_STANDARD.md §2)", rel, key)
				failures++
				continue
			}
			if val == "" {
				t.Errorf("%s: front-matter key %q is present but empty", rel, key)
				failures++
				continue
			}
			if allowed, closed := frontMatterClosedSets[key]; closed {
				if !contains(allowed, val) {
					t.Errorf("%s: front-matter key %q has value %q, which is not in the closed set %v "+
						"(DOCS_STANDARD.md §2). If this value genuinely belongs, widen the standard AND "+
						"this test's closed set in the same change.", rel, key, val, allowed)
					failures++
				}
			}
		}
	}

	if failures == 0 && testing.Verbose() {
		t.Logf("checked %d docs/**.md files, all conform", len(files))
	}
}

// parseFrontMatterBlock parses the leading `---`-delimited YAML-ish block
// this repo's docs use: line-by-line `key: value`, split on the first `:`.
// It intentionally does not pull in a YAML library -- the front-matter
// contract is a flat set of scalar keys, and a real YAML parser would
// silently accept structures (nested maps, lists) the standard does not
// define, defeating the point of a closed-set gate.
func parseFrontMatterBlock(content string) (map[string]string, bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return nil, false
	}
	block := map[string]string{}
	for _, raw := range lines[1:] {
		line := strings.TrimRight(raw, "\r")
		if line == "---" {
			return block, true
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		val = strings.Trim(val, `"'`)
		if key != "" {
			block[key] = val
		}
	}
	// Reached EOF without a closing `---`: not a valid block.
	return nil, false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestDocsRootLayoutIsClosed enforces DOCS_STANDARD.md §1: docs/ itself
// carries only the standard and its own directory index, and every doc
// (public or internal) lives under public/ or internal/. A stray file at
// docs/ROOT.md is invisible to both halves of the pipeline: the site build
// only walks docs/public/**, and DOCS_STANDARD.md's own bucket table (§3)
// has no "repo root of docs/" row, so nobody's classification logic ever
// looks at it. Task 7 relies on this layout staying closed.
//
// FALSE-POSITIVE ESCAPE HATCH: a genuinely new root-level governance file
// (in the spirit of DOCS_STANDARD.md and CLAUDE.md, not a stray design or
// planning doc) should be added to docsRootAllowed in the same change that
// adds it, with a comment saying why it belongs at the root rather than
// under public/ or internal/.
var docsRootAllowed = map[string]bool{
	"docs/DOCS_STANDARD.md": true,
	"docs/CLAUDE.md":        true,
}

func TestDocsRootLayoutIsClosed(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "docs").Output()
	if err != nil {
		t.Fatalf("git ls-files -z docs: %v", err)
	}
	var checked int
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || path.Ext(rel) != ".md" {
			continue
		}
		// Only files DIRECTLY under docs/ (no further slash) are in scope --
		// this test is about the root, not the whole tree.
		if strings.Contains(strings.TrimPrefix(rel, "docs/"), "/") {
			continue
		}
		checked++
		if !docsRootAllowed[rel] {
			t.Errorf("%s: a stray *.md directly under docs/. DOCS_STANDARD.md §1 puts every doc "+
				"under docs/public/ or docs/internal/; only DOCS_STANDARD.md and CLAUDE.md belong at "+
				"the docs/ root itself. Move it per §3's bucket table, or add it to docsRootAllowed "+
				"with a comment if it is genuinely a new root governance file.", rel)
		}
	}
	if checked == 0 {
		t.Fatal("found no *.md directly under docs/ at all -- this gate would verify nothing. " +
			"Either docs/DOCS_STANDARD.md and docs/CLAUDE.md were both moved/deleted, or the scan " +
			"logic above stopped matching root-level paths.")
	}
}
