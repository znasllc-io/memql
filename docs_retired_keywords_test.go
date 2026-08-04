package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestDocsDoNotTeachRetiredDeclarationKeywords is the sibling of
// TestDocsDoNotReferencePrefixedConstructNames (#2914 / #2917 / #2979), for the
// half that gate structurally cannot reach.
//
// # Why the name gate cannot catch this
//
// That gate polices prefixed construct NAMES, and it is deliberately narrow: it
// fires only when the tree declares `X` with the kind the prefix asserts. That
// narrowness is what keeps it silent on counter-examples and on bundle-owned
// names, and it is why `mutation space mutationCreateSpace` slipped past --
// `createSpace` is bundle-owned, declared nowhere in this repo, so the name half
// is correctly undecidable.
//
// But the KEYWORD is wrong independently of whether the name resolves. It is a
// syntax claim, not a name claim, and no amount of widening the name gate
// reaches it (memql#2974).
//
// # Why it earns a gate
//
// `mutation` was renamed to `mutate` in memql#2041, and
// component/language/dslspec hard-fails if it is still a construct keyword --
// so a doc teaching it teaches a declaration the parser REJECTS. CLAUDE.md is
// the sharp edge: the drifted line was the canonical "Mutations:" example in its
// Functions section, which is the standing instruction every Claude Code session
// in this repo reads. A worked example there teaching a retired form is
// self-reinforcing -- the instructions produce the drift the gates then reject.
//
// # Declaration position only
//
// The prose word "mutation" is everywhere and legitimate ("the mutation
// writes...", "Mutations:"), so this anchors on the two-identifier-then-brace
// declaration shape at the start of a line rather than on the bare word.
//
// # Scope: markdown, and why not wider
//
// The sibling name gate sweeps every tracked file, and widening this one to
// match was measured rather than assumed. It cannot be, yet: the only
// non-markdown hits of the declaration shape are two fixtures in
// component/memql/callgraph/callgraph_test.go, and they are load-bearing --
// that checker's own regex still matches the retired keyword, so rewriting the
// fixtures turns its tests red. That is memql#3043, filed with the measurement.
// Widening this gate belongs in the same change that fixes the checker, not
// before it; a gate that requires a broken test or a file exemption to go green
// is worse than one with a stated boundary.
//
// # Escape hatch
//
// A doc teaching AGAINST the retired form needs to be able to quote it, and the
// honest advice is narrower than it first looks: **indenting does not help**.
// The pattern allows leading whitespace on purpose (an indented or fenced code
// block is exactly where a declaration gets taught), so an indented example is
// still a violation -- the fixture test below pins that in both directions.
//
// What works is keeping the line out of DECLARATION shape: quote the keyword
// inline in prose, or show the declaration with the LIVE keyword and describe
// the retired spelling in words around it. If a doc ever genuinely has to
// display the whole retired declaration, follow the sibling gate's convention
// and add a narrow per-path exemption (prefixNameGateExempt in
// docs_construct_names_test.go) rather than weakening the pattern -- and keep
// that list as short as it keeps its own.
//
// # Known gaps, measured rather than assumed
//
// Two, recorded here because a gate whose limits live only in a PR comment is a
// gate whose next reader believes it covers more than it does:
//
//   - OTHER RETIRED FORMS. memql#2974 asks whether the retired `func (Query)` /
//     `func (Mutation)` receiver forms belong in the same sweep. Measured on
//     this tree: `func (Query)` has ZERO declaration-shaped markdown hits, and
//     `func (Mutation)` has THREE, all in
//     docs/internal/planning/shape-drift-hardening.md (:127, :138, :151), under
//     the headings "Status quo", "Patch alternative" and an official-helper
//     proposal -- prescriptive, not historical quotation. They are NOT gated
//     here: the receiver form is a different declaration shape than the
//     two-identifier one this table can express, and gating it means rewriting
//     a planning document's proposal blocks. Tracked separately rather than
//     done halfway.
//   - PR-TIME COVERAGE. .github/workflows/ci.yml routes lanes by changed-path
//     bucket for `pull_request` events, and no bucket matches `**/*.md`, so a
//     docs-only PR skips go-checks and never runs this gate. Drift still cannot
//     LAND undetected -- that routing applies to pull_request only, and
//     `merge_group` runs every lane unconditionally, so the merge queue catches
//     it -- but the feedback arrives at the queue rather than on the PR. The
//     sibling name gate has the identical hole, and widening the filter makes
//     every docs-only PR pay the full Go suite, which is a CI-spend call rather
//     than this gate's to make.
func TestDocsDoNotTeachRetiredDeclarationKeywords(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(string(out), "\x00")

	var scanned int
	seen := map[string]bool{}
	for _, rel := range files {
		if rel == "" {
			continue
		}
		data, readErr := os.ReadFile(rel)
		if readErr != nil {
			// A tracked-but-absent path is somebody else's problem; skipping it
			// silently is what would be wrong, so say so.
			t.Logf("skipping unreadable tracked file %s: %v", rel, readErr)
			continue
		}
		scanned++
		seen[rel] = true
		lines := strings.Split(string(data), "\n")
		for _, r := range retiredDeclarationKeywords {
			re := retiredDeclRE(r.keyword)
			for i, line := range lines {
				if !re.MatchString(line) {
					continue
				}
				t.Errorf("%s:%d teaches the retired `%s` declaration keyword -- it was renamed to "+
					"`%s` in %s, and component/language/dslspec hard-fails if it is still a "+
					"construct keyword, so this is a declaration the parser REJECTS.\n"+
					"  %s\n"+
					"Write `%s` instead. If this line is deliberately quoting the old form to teach "+
					"against it, note that INDENTING DOES NOT HELP -- the pattern allows leading "+
					"whitespace, because an indented code block is exactly where a declaration "+
					"gets taught. Quote the keyword inline in prose instead, or show the "+
					"declaration with the live keyword and describe the retired spelling around "+
					"it. If the whole retired declaration genuinely has to be displayed, add a "+
					"narrow per-path exemption the way prefixNameGateExempt does in "+
					"docs_construct_names_test.go rather than weakening this pattern.",
					rel, i+1, r.keyword, r.replacement, r.ref, strings.TrimRight(line, "\r"),
					r.replacement)
			}
		}
	}

	// A sweep that silently stops resolving files reports clean forever, so the
	// floor guards against the glob or the git invocation breaking.
	//
	// A COUNT alone is not enough, and that is not hypothetical: narrowing the
	// glob to `docs/*.md` still resolves 123 files -- comfortably over any round
	// number -- while dropping CLAUDE.md, which this gate's own rationale names
	// as the sharp edge. So the sentinels below assert that the specific files
	// the gate exists for were actually read, which a count structurally cannot
	// express.
	if scanned < 50 {
		t.Fatalf("only %d markdown files scanned -- the sweep has stopped resolving them and this "+
			"gate would now pass vacuously", scanned)
	}
	for _, sentinel := range []string{
		"CLAUDE.md",
		"docs/public/language/authoring-rules.md",
	} {
		if !seen[sentinel] {
			t.Fatalf("%s was not scanned (%d files were) -- the sweep has narrowed and this gate "+
				"is no longer covering the docs it exists for. A file count cannot catch that, "+
				"which is why these sentinels are asserted by name.", sentinel, scanned)
		}
	}
}

// retiredDeclarationKeywords is one entry per retired declaration keyword, with
// what replaced it. The shape is `^<keyword> <ident> <ident> {` -- the
// two-identifier signature form every construct of this class uses.
//
// Package-level, and read by BOTH the sweep and the fixture test below, so a
// second entry inherits the fire-proof automatically. An entry whose
// declaration shape is NOT the two-identifier form (the retired
// `func (Mutation) name(args any) error {` receiver form, say) cannot be
// expressed here at all -- see "Known gaps" above; that needs a pattern per
// entry rather than a keyword per entry.
var retiredDeclarationKeywords = []struct{ keyword, replacement, ref string }{
	{"mutation", "mutate", "memql#2041"},
}

// retiredDeclRE builds the declaration-position pattern for one retired keyword.
//
// ONE definition, called by the sweep and by the fixture test. It was two
// separate literals, and that is not a style point: sabotaging the sweep's copy
// left the fixture test green, the suite green, and a doc teaching the retired
// keyword sitting in the tree -- measured during the memql#3044 review. A pin
// test carrying its own copy of the pattern pins a string that exists only
// inside itself, which is the exact silent-disable shape this file was written
// to prevent.
//
// QuoteMeta because the keyword is interpolated into a pattern. `mutation` is
// inert either way, but the obvious next candidate is `func (Mutation)`, whose
// parentheses are balanced regex metacharacters -- it would compile cleanly and
// match nothing, so the lane would ship dead AND green.
func retiredDeclRE(keyword string) *regexp.Regexp {
	return regexp.MustCompile(`^[ \t]*` + regexp.QuoteMeta(keyword) +
		`[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)
}

// The gate must actually fire. Without this the regex could be wrong in a way
// that makes it match nothing, and the sweep above would report clean forever --
// the silent-disable shape the sibling gate's own resolver self-check exists to
// catch.
//
// It exercises retiredDeclRE, the SAME constructor the sweep calls, and
// iterates retiredDeclarationKeywords rather than hardcoding one keyword. Both
// matter: the first is what makes this a pin rather than a self-referential
// assertion, the second is what stops a second table entry shipping with no
// coverage at all.
func TestRetiredKeywordGateMatchesADeclarationAndNotProse(t *testing.T) {
	for _, r := range retiredDeclarationKeywords {
		re := retiredDeclRE(r.keyword)

		for _, bad := range []string{
			r.keyword + " space mutationCreateSpace {",
			"  " + r.keyword + " participant addAgentToSpace {",
			"\t" + r.keyword + " lead mutationTouchLead {",
			// Indented inside prose -- pinned as a VIOLATION so the failure
			// message can never again advise indenting as the escape hatch.
			"    " + r.keyword + " space createSpace {",
		} {
			if !re.MatchString(bad) {
				t.Errorf("the %s gate does not match a retired declaration, so it would report "+
					"clean forever: %q", r.keyword, bad)
			}
		}

		for _, ok := range []string{
			r.replacement + " space createSpace {",       // the live form
			"The mutation writes exactly one aggregate.", // prose
			"Mutations:", // a heading
			"| `mutation` | retired, use `mutate` | ",          // a table quoting the keyword
			"a `mutation` declaration used to look like this:", // teaching against it
			"  ev: mutation createSpawnEvent(nodeId: args.id)", // a CALL, not a declaration
			"mutation {", // not the two-identifier shape
			// The two with real discriminating power. Every fixture above
			// survives an anchor being "simplified" away; these do not --
			// dropping `\{` makes the first fire, dropping `^` the second.
			"the mutation space handler runs first",   // no brace: needs the `\{` anchor
			"see `x.mutation space createSpace {` in", // mid-line: needs the `^` anchor
		} {
			if re.MatchString(ok) {
				t.Errorf("the %s gate fires on a legitimate line, which is how a gate gets "+
					"suppressed and then stops catching the real case: %q", r.keyword, ok)
			}
		}
	}
}
