package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
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
// One, recorded here because a gate whose limits live only in a PR comment is a
// gate whose next reader believes it covers more than it does:
//
//   - PR-TIME COVERAGE. .github/workflows/ci.yml routes lanes by changed-path
//     bucket for `pull_request` events, and no bucket matches `**/*.md`, so a
//     docs-only PR skips go-checks and never runs this gate. Drift still cannot
//     LAND undetected -- that routing applies to pull_request only, and
//     `merge_group` runs every lane unconditionally, so the merge queue catches
//     it -- but the feedback arrives at the queue rather than on the PR. The
//     sibling name gate has the identical hole, and widening the filter makes
//     every docs-only PR pay the full Go suite, which is a CI-spend call rather
//     than this gate's to make. Tracked as memql#3054.
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

		// ARM 2: the retired procedural RECEIVER form (memql#3053). Delegated
		// to the parser rather than tabled here -- see retiredReceiverFormRef.
		if !retiredReceiverGateExempt(rel) {
			for i, line := range lines {
				if err := parser.RejectLegacyProceduralAuthorForm(line); err == nil {
					continue
				}
				t.Errorf("%s:%d teaches the retired procedural RECEIVER form -- "+
					"component/language/parser.RejectLegacyProceduralAuthorForm refuses this exact "+
					"line, so it is a declaration the parser REJECTS (%s).\n"+
					"  %s\n"+
					"Write the struct form instead: `<kind> <Concept> <name> { args { ... } ... }`. "+
					"As with the keyword arm, INDENTING DOES NOT HELP -- the parser's own pattern "+
					"allows leading whitespace. If this doc genuinely has to display the retired "+
					"declaration to teach against it or to record it as history, add a narrow "+
					"per-path entry to retiredReceiverGateExempt with the reason, the way "+
					"prefixNameGateExempt does in docs_construct_names_test.go.",
					rel, i+1, retiredReceiverFormRef, strings.TrimRight(line, "\r"))
			}
		}

		// ARM 1: retired declaration KEYWORDS, two-identifier shape.
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
// declaration shape is NOT the two-identifier form -- the retired
// `func (Mutation) name(args any) error {` receiver form -- cannot be expressed
// here at all. That is arm 2, and it is not a second table; see
// retiredReceiverFormRef.
var retiredDeclarationKeywords = []struct{ keyword, replacement, ref string }{
	{"mutation", "mutate", "memql#2041"},
}

// retiredReceiverFormRef names the ruling arm 2 enforces, for the diagnostic.
//
// # Why arm 2 is delegation and not a second table
//
// memql#3053 asked for "a pattern per entry rather than a keyword per entry",
// which would have meant tabling the twelve retired receiver names here. It is
// not tabled, because the parser already owns that list and enforces it:
// component/language/parser.RejectLegacyProceduralAuthorForm is what the DSL
// load path calls, and its pattern is pinned to the twelve receiver names the
// engine has ever accepted (Query, Mutation, Logic, Spec, Automation, Builtin,
// Prompt, Provider, Shape, Tool, Policy, Seed).
//
// A copy here would be a THIRTEENTH definition of "what the parser rejects",
// free to drift from the twelve. That is the same defect this file's own
// retiredDeclRE comment was written about -- a pin that pins a string existing
// only inside itself -- and it is what makes the gate's claim weaker: a
// hand-rolled regex asserts "this looks retired to me", while calling the
// parser asserts "the parser refuses this exact line". The second is the claim
// worth making, and it cannot go stale: a receiver name added to or removed
// from the parser's list changes this gate on the same commit.
//
// The cost is honest and small: that function also blanks comments before
// scanning (memql#1074), so a receiver form appearing only inside a `//` or
// `/* */` comment is not reported here either. That matches the parser exactly,
// which is the point -- a doc showing something the parser tolerates is not
// teaching a declaration that fails to load.
const retiredReceiverFormRef = "memql#303 retired the author-facing procedural surface"

// retiredReceiverGateExempt reports whether a path is outside arm 2's sweep.
//
// Mirrors prefixNameGateExempt in docs_construct_names_test.go, including its
// warning: what does NOT belong here is a file that merely happens to contain a
// violation. Every entry below must WRITE the retired form in order to say what
// it forbids or to record what was once true, which is the only reason the
// sibling list accepts one.
//
// Per-path rather than per-(path, receiver kind), and that coarseness is a real
// cost worth stating: exempting authoring-rules.md for `func (Shape)` also
// buys silence on a future `func (Tool)` in the same file. It is accepted here
// because it is the convention the sibling gate already set and a second,
// finer-grained exemption mechanism is a design decision this change should not
// make unilaterally. Note the KEYWORD arm still polices every file below --
// only arm 2 is skipped -- so an exempt path is not unpoliced.
func retiredReceiverGateExempt(rel string) bool {
	switch rel {
	case "docs/public/language/authoring-rules.md":
		// The canonical authoring reference, and the two hits are the
		// counter-examples it exists to publish: a `func (Shape)` block under
		// "Retired forms (rejected at parse time)" carrying `// REJECTED --`,
		// and a `func (Spec)` block under "**Wrong (rejected at
		// registration):**". A rule against a form cannot be stated without
		// showing the form.
		return true
	case "docs/internal/design/dsl-syntax-audit-964.md":
		// A syntax AUDIT: its whole subject is which spellings are live and
		// which are not. The `func (Policy)` hit is explicitly labelled
		// "documented-but-not-live form (decision policy)" -- and that tier is
		// itself retired (memql#984), so the block is a record of something
		// that never shipped rather than instruction.
		return true
	case "docs/internal/planning/shape-drift-hardening.md":
		// An UNSHIPPED design note (`status: draft`, "Proposed. Not shipped."),
		// whose three `func (Mutation)` blocks argue full-replace vs merge
		// semantics. Exempt rather than rewritten, deliberately: the blocks'
		// BODIES are pre-#303 procedural too (`return insert({...})`, `get()`,
		// `merge()`), so rewriting only the receiver line yields source that is
		// still un-loadable while now LOOKING current, and rewriting them fully
		// means inventing live syntax for a `patch()` helper that does not
		// exist -- a design call on someone else's open proposal, not a
		// mechanical swap. memql#3053 anticipated exactly this split. The
		// document instead carries a note at that section pointing readers at
		// the live form, so the "reader implements the proposal and writes a
		// declaration that cannot load" harm is addressed without this change
		// silently editing what the proposal claims.
		return true
	}
	return false
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

// The same fire-proof for arm 2. Without it the receiver arm could be wired to
// something that never reports, and the sweep would be green forever -- the
// silent-disable shape this whole file exists to prevent, which a delegated arm
// does not get a pass on just because the callee is trusted.
//
// It exercises parser.RejectLegacyProceduralAuthorForm, the SAME function the
// sweep calls, and it iterates the twelve receiver names so that a name dropped
// from the parser's list fails HERE, loudly, rather than quietly shrinking the
// gate. That is the whole benefit of delegating claimed above; it is only real
// if something checks it.
func TestRetiredReceiverGateMatchesADeclarationAndNotProse(t *testing.T) {
	receivers := []string{
		"Query", "Mutation", "Logic", "Spec", "Automation", "Builtin",
		"Prompt", "Provider", "Shape", "Tool", "Policy", "Seed",
	}

	for _, kind := range receivers {
		for _, bad := range []string{
			"func (" + kind + ") name(args any) error {",
			"  func (" + kind + ") name(args any) error {",
			"\tfunc (" + kind + ") name {",
			// Indented inside prose -- a VIOLATION, pinned in both arms so the
			// failure message can never again advise indenting as the way out.
			"    func (" + kind + ") name {",
			// Spacing the parser tolerates. These are the exact spellings that
			// sailed past an earlier, narrower rejection (memql#2853 round 3),
			// so a doc gate that missed them would repeat that hole one layer up.
			"func  (" + kind + ") name {",
			"func ( " + kind + " ) name {",
		} {
			if err := parser.RejectLegacyProceduralAuthorForm(bad); err == nil {
				t.Errorf("the receiver gate does not reject a retired declaration, so it would "+
					"report clean forever: %q", bad)
			}
		}
	}

	for _, ok := range []string{
		"mutate space createSpace {",                           // the live form
		"query participant spaceParticipants {",                // the live form
		"The `func (Mutation)` receiver form is retired.",      // prose
		"| `func (Shape)` | retired, use `shape <C> <n>` |",    // a table quoting it
		"see `func (Spec) example` in the old tree",            // mid-line prose
		"func (myType) Method() error {",                       // ordinary Go, not a DSL receiver
		"func (e *MemQLEngine) embedActionIntent(id string) {", // ordinary Go
	} {
		if err := parser.RejectLegacyProceduralAuthorForm(ok); err != nil {
			t.Errorf("the receiver gate fires on a legitimate line, which is how a gate gets "+
				"suppressed and then stops catching the real case: %q (%v)", ok, err)
		}
	}
}

// The exemption list must name paths that EXIST. A typo'd or renamed entry is
// silently inert -- it exempts nothing, so the gate fires on a file whose
// exemption was meant to be in place, and whoever hits that failure has no way
// to tell a deliberate rule from a stale string. Docs get renamed; this is the
// cheap check that keeps the list honest.
func TestRetiredReceiverExemptionsNameRealFiles(t *testing.T) {
	for _, rel := range []string{
		"docs/public/language/authoring-rules.md",
		"docs/internal/design/dsl-syntax-audit-964.md",
		"docs/internal/planning/shape-drift-hardening.md",
	} {
		if !retiredReceiverGateExempt(rel) {
			t.Errorf("%s is listed here but retiredReceiverGateExempt does not exempt it -- the "+
				"two lists have drifted", rel)
		}
		if _, err := os.Stat(rel); err != nil {
			t.Errorf("%s is exempt from the receiver gate but does not exist: %v. A stale "+
				"exemption is silently inert.", rel, err)
		}
	}
	if retiredReceiverGateExempt("CLAUDE.md") {
		t.Error("CLAUDE.md must NOT be exempt -- it is the standing instruction every session " +
			"reads, and this gate's whole rationale names it as the sharp edge")
	}
	if retiredReceiverGateExempt("integrations/CLAUDE.md") {
		t.Error("integrations/CLAUDE.md must NOT be exempt -- it taught the retired " +
			"`func (Builtin)` form prescriptively, which is the violation memql#3053 " +
			"was filed to remove, not to grandfather")
	}
}
