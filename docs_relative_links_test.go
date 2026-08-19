package main

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocsRelativeLinksResolve gates every in-repo relative markdown link
// under the repo root and docs/ against the file it actually names
// (memql#4087, the repo-cleanup-docs-update campaign's Task 1). A relative
// link that has gone dead is invisible to the reader until they click it --
// unlike a `go build` failure, nothing about a stale prose reference makes
// noise, so it survives the file move / rename / delete that broke it
// indefinitely.
//
// The probe that motivated this gate found four independent SHAPES of dead
// link over docs/**.md and the repo-root *.md files: a target deleted in a
// docs scrub with nothing repointed at its replacement (docs/polyphon-
// architecture.md, 05-current-state-map.md); a path that only resolves from
// the REPO ROOT rather than from the linking file's own directory (~40 links
// in docs/internal/{design,planning}/*.md pointing at component/ and dsl/
// paths with too few or zero leading `../`); a same-repo path left stale by
// THIS campaign's own docs/-root stray moves (docs/internal/ops/agent-stack.md
// and ci-design.md, whose links were correct only from their old docs/-root
// location); and two links into a SIBLING repo (memql-cockpit,
// DEVOPS_DSL_BUNDLE_HANDOFF.md) that were never resolvable from inside this
// tree and have to be prose, not markdown links, to say so honestly.
//
// # What this deliberately does NOT catch
//
//   - Anchor (`#fragment`) validity within a resolved target file. A link to
//     `functions.md#some-heading` is checked only down to `functions.md`
//     existing; whether that file still has a heading matching `some-heading`
//     is unchecked. Anchors drift with every heading rename in the target
//     file, which would make this gate fail on documentation elsewhere
//     changing wording, not on the linking file itself -- a much noisier
//     signal for a much smaller payoff than the path check above.
//   - Links to a sibling repo, a URL, or any target this repo cannot resolve
//     from its own tree. Those are skipped by scheme/prefix, not verified --
//     see docsLinkSkippable below.
//   - A malformed link target containing a literal space or other character
//     that breaks `[text](target)` syntax outright (Markdown itself would
//     render such a "link" as literal text in most renderers). The
//     specified extraction pattern requires the parenthesized run to contain
//     no whitespace, so a target like `(03-epic-decouple-the product.md)`
//     is invisible to this gate the same way it is invisible to a strict
//     Markdown renderer's link syntax -- it never becomes a clickable link
//     in the first place, so there is no dead link to report. (One instance
//     of this shape was fixed by hand during the corpus sweep that produced
//     this gate; a future one needs the same manual eye.)
//
// FALSE-POSITIVE ESCAPE HATCH: a regex or path-shaped literal inside inline
// single-backtick code (not a real link) can look like `[text](target)` --
// the probe run while writing this gate hit exactly one, a DNS-label regex
// in authoring-rules.md whose `(?:...)` non-capturing group reads as
// bracket-then-paren. Inline code spans are stripped before link extraction
// for exactly this reason (see stripInlineCode). If a genuine new
// false-positive shape turns up, widen the stripping/extraction logic here
// rather than special-casing the file -- the same "known sites only misses
// the next paraphrase" lesson the lifecycle-docs gate's review history
// recorded (lifecycle_docs_conformance_test.go).
func TestDocsRelativeLinksResolve(t *testing.T) {
	files := gitTrackedLinkCheckFiles(t)
	if len(files) == 0 {
		t.Fatal("found no root or docs/**.md files to check -- this gate would verify nothing.")
	}

	var checked int
	for _, rel := range files {
		data, err := os.ReadFile(rel)
		if err != nil {
			// git-tracked but locally absent (partial checkout): not repo drift.
			continue
		}
		dir := filepath.Dir(rel)
		for _, target := range extractRelativeLinkTargets(string(data)) {
			checked++
			resolved := filepath.Join(dir, target)
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s: dead link -> %q (resolved to %s, which does not exist)", rel, target, resolved)
			}
		}
	}

	if checked == 0 {
		t.Fatal("checked 0 links across every root/docs/**.md file -- either every doc stopped " +
			"linking anywhere, or extractRelativeLinkTargets stopped recognising markdown link " +
			"syntax. A gate that examines nothing passes forever.")
	}
}

// gitTrackedLinkCheckFiles enumerates the files this gate checks: every
// tracked *.md directly at the repo root, plus every tracked docs/**.md,
// excluding the same two trees the front-matter gate exempts
// (docsFrontMatterExempt, docs_front_matter_test.go) for the same reasons --
// docs/superpowers/** quotes banned/pre-standard forms by design, and
// docs/public/reference/_generated/** is machine-generated at release time.
func gitTrackedLinkCheckFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files -z: %v", err)
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || path.Ext(rel) != ".md" {
			continue
		}
		atRoot := !strings.Contains(rel, "/")
		underDocs := strings.HasPrefix(rel, "docs/")
		if !atRoot && !underDocs {
			continue
		}
		if docsFrontMatterExempt(rel) {
			continue
		}
		files = append(files, rel)
	}
	return files
}

// fencedCodeBlock matches a complete ```-delimited fenced block, including
// the fence lines themselves, non-greedily so adjacent blocks stay distinct.
var fencedCodeBlock = regexp.MustCompile("(?s)```.*?```")

// inlineCodeSpan matches a single-backtick-delimited run with no backtick or
// newline inside -- deliberately narrow. This repo's docs never nest a real
// markdown link inside inline code (a code span containing literal
// `[text](url)` characters renders as that literal text, not as a link), so
// stripping these before link extraction removes false positives -- prose
// regexes, glob patterns, shell fragments shaped like `[x](y)` -- without
// touching any link the surrounding prose actually means to be clickable.
var inlineCodeSpan = regexp.MustCompile("`[^`\n]*`")

// linkOrImageTarget extracts the parenthesized target of a markdown link or
// image: `[text](target)`. The target run excludes `)` and whitespace, which
// is also why a target containing a literal space is invisible to this
// extractor -- see the false-positive/does-not-catch discussion on
// TestDocsRelativeLinksResolve.
var linkOrImageTarget = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// extractRelativeLinkTargets returns every link target in content that
// names an in-repo relative path: fenced and inline code are stripped
// first, then http(s)/mailto/anchor-only/scheme-containing targets are
// skipped, then any `#fragment` suffix is trimmed (anchor validity is out
// of scope, per the doc comment above).
func extractRelativeLinkTargets(content string) []string {
	stripped := fencedCodeBlock.ReplaceAllString(content, "")
	stripped = inlineCodeSpan.ReplaceAllString(stripped, "")

	var targets []string
	for _, m := range linkOrImageTarget.FindAllStringSubmatch(stripped, -1) {
		target := m[1]
		if docsLinkSkippable(target) {
			continue
		}
		if idx := strings.Index(target, "#"); idx >= 0 {
			target = target[:idx]
		}
		if target == "" {
			continue
		}
		targets = append(targets, target)
	}
	return targets
}

// docsLinkSkippable reports whether a link target is out of scope for
// resolution: an external URL, a mailto link, a same-file anchor, or any
// target containing a scheme separator.
func docsLinkSkippable(target string) bool {
	switch {
	case strings.HasPrefix(target, "http://"),
		strings.HasPrefix(target, "https://"),
		strings.HasPrefix(target, "mailto:"),
		strings.HasPrefix(target, "#"),
		strings.Contains(target, "://"):
		return true
	default:
		return false
	}
}
