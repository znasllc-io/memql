package dslconformance

import (
	"fmt"
	"github.com/znasllc-io/memql/dsl"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/dslclause"
	"github.com/znasllc-io/memql/core/dslfs"
)

// clause_opener_convention_test.go -- memql#2863.
//
// A struct-query clause keyword must be followed by a PLAIN ASCII SPACE or TAB
// in authored `.memql`. This is a tree convention, enforced here, and it exists
// because the alternative does not work.
//
// # Why a convention gate rather than a wider scanner
//
// The engine's normaliser separates keyword from body with `strings.TrimSpace`
// and, for `filter`, a bare `HasPrefix` -- so it accepts `filter(...)`,
// `filter<NBSP>...`, and every other unicode space. FOUR openers walk the same
// text line by line and must agree about where a clause begins:
//
//	component/memql/sense/row_intrinsic_scan.go   the IDE + gate scanner
//	test/dslconformance/conformance_test.go                       walkFilterPredicates
//	component/language/pagination/checker.go      the pagination analysis
//	component/language/dslclause                  the shared answer (#2815)
//
// Teaching only SOME of them a spelling makes them contradict each other. That
// was measured, not theorised: widening just the sense scanner to accept `(`
// meant a fully compliant query --
//
//	filter(row.id==args.x && ownerUserId==actor.userId)
//
// -- passed the widened scanner and FAILED TestPerRowAuthzClassification, whose
// opener did not see the clause and therefore could not see the caller-check
// sitting inside it. The author had no spelling that satisfied both gates.
//
// The unicode-space half IS fixed in the scanners, in dslclause.StartsWith so
// every consumer inherits it at once. The `(` half is closed HERE instead,
// because `filter(` buys an author nothing and costs every gate a special case.
// #2817 set this precedent: its brace-line shape was closed by a tree gate
// (TestClauseDoesNotShareTheOpeningBraceLine) rather than by teaching the
// scanners the shape.
//
// # What is NOT claimed
//
// This does not close every spelling the engine accepts. `rewriter.go`'s filter
// opener is a bare `strings.HasPrefix(line, "filter")` with NO boundary check
// at all, so `filterid == args.x` and `filter!isDeleted` normalise fine and no
// opener sees them. Those are dropped-space typos rather than exotic pastes;
// they are recorded in #2863 as remaining work rather than papered over here,
// because a gate that claims completeness it does not have is the failure mode
// this issue is about.

// clauseKeywords are the struct-query directives whose separator this gate
// polices. Sourced from dslclause so a new directive is covered automatically.
var clauseKeywords = dslclause.StructQueryDirectives

// clauseOpenerVerdict classifies a trimmed line: which clause keyword it opens
// (if any) and whether the separator is the convention.
//
// ONE function, called by both the tree gate and the fixture test. It was two
// hand-copies, and review proved that duplication is not cosmetic: adding
// `|| r == '('` to the gate's boundary skip -- which neuters `filter(` detection
// entirely -- left BOTH tests passing. The companion test, whose whole purpose
// is that "the tree is clean, so the gate cannot demonstrate it works", could
// not detect a gate that had stopped working. That is the
// test-re-implements-the-code defect in its purest form, and the comment
// claiming "it re-implements nothing" was false.
func clauseOpenerVerdict(trimmed string) (keyword, verdict string) {
	for _, kw := range clauseKeywords {
		rest, ok := strings.CutPrefix(trimmed, kw)
		if !ok || rest == "" {
			continue
		}
		r, _ := utf8.DecodeRuneInString(rest)
		// An identifier that merely STARTS with the keyword (`filterMode`,
		// `sortOrder`) is not an opener. That boundary rule is why
		// dslclause.StartsWith exists rather than a bare HasPrefix.
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			continue
		}
		switch {
		case r == ' ' || r == '\t':
			return kw, "ok"
		case r == '(':
			return kw, "paren"
		case unicode.IsSpace(r):
			return kw, "unicode-space"
		default:
			// Anything else is not a clause opener at all (`filter:` as a
			// mutation insert key, `filter}`). Deliberately tolerated -- this is
			// a convention gate, not a syntax checker.
			return kw, "not-an-opener"
		}
	}
	return "", "not-an-opener"
}

// TestClauseOpenerUsesAPlainSpace is the convention gate.
func TestClauseOpenerUsesAPlainSpace(t *testing.T) {
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var findings []string
	scanned := 0

	for _, p := range paths {
		src := readTreeFile(t, p)
		for i, raw := range strings.Split(blankComments(src), "\n") {
			trimmed := strings.TrimSpace(raw)
			if trimmed == "" {
				continue
			}
			kw, verdict := clauseOpenerVerdict(trimmed)
			if kw == "" || verdict == "not-an-opener" {
				continue
			}
			scanned++
			switch verdict {
			case "paren":
				findings = append(findings, fmt.Sprintf(
					"%s:%d  `%s(` -- for `filter`, write `filter ` (a plain space). The engine "+
						"accepts the paren form, but only some of the four clause openers do, so "+
						"a construct written this way is invisible to "+
						"TestPerRowAuthzClassification and to the pagination analysis while still "+
						"being scanned by the IDE. For `sort` the paren is rejected by the grammar "+
						"outright, and a plain space alone does NOT fix it -- write "+
						"`sort \"key\", \"dir\"`.",
					p, i+1, kw))
			case "unicode-space":
				r, _ := utf8.DecodeRuneInString(strings.TrimPrefix(trimmed, kw))
				findings = append(findings, fmt.Sprintf(
					"%s:%d  `%s` is followed by U+%04X, not a plain space. The engine accepts it "+
						"(its normaliser uses strings.TrimSpace), which is exactly why it is worth "+
						"forbidding here -- it is almost always an invisible paste artifact rather "+
						"than an intent",
					p, i+1, kw, r))
			}
		}
	}

	if scanned == 0 {
		t.Fatal("scanned 0 clause openers across the tree -- this gate would then pass vacuously, " +
			"which is the meaningless-zero failure mode (#2799). Check clauseKeywords against " +
			"dslclause.StructQueryDirectives and the blankComments call")
	}

	sort.Strings(findings)
	for _, f := range findings {
		t.Errorf("%s", f)
	}
	t.Logf("checked %d clause opener(s) across %d file(s)", scanned, len(paths))
}

// TestClauseOpenerGateCatchesTheForbiddenSpellings is the failing-first half:
// the tree is clean, so the gate above cannot demonstrate that it works.
//
// It re-implements nothing -- it runs the same classification the gate runs, on
// fixtures, so a change to the rule is visible in both places at once.
func TestClauseOpenerGateCatchesTheForbiddenSpellings(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"filter row.id==args.x", "ok"},
		{"filter\trow.id==args.x", "ok"},
		{"filter(row.id==args.x)", "paren"},
		{"sort(\"row.createdAt\", \"desc\")", "paren"},
		{"filter\u00a0row.id==args.x", "unicode-space"},
		{"filter\u2003row.id==args.x", "unicode-space"},
		{"filter\u3000row.id==args.x", "unicode-space"},
		// Identifiers that merely start with a keyword are NOT openers -- the
		// boundary rule. Getting it wrong would flag ordinary fields.
		{"filterMode string!", "not-an-opener"},
		{"sortOrder string", "not-an-opener"},
		{"filterable boolean", "not-an-opener"},
		{"filter_by string", "not-an-opener"},
		{"count1 integer", "not-an-opener"},
		{"shapeName string", "not-an-opener"},
		// Not openers either, and deliberately tolerated rather than reported.
		{"filter: args.filter", "not-an-opener"},
		{"filter-mode string", "not-an-opener"},
	} {
		if _, got := clauseOpenerVerdict(tc.in); got != tc.want {
			t.Errorf("clauseOpenerVerdict(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClauseKeywordsComeFromDslclause pins the source of the keyword list, so a
// new struct-query directive is policed the day it lands rather than whenever
// someone remembers this file.
func TestClauseKeywordsComeFromDslclause(t *testing.T) {
	if len(clauseKeywords) == 0 {
		t.Fatal("clauseKeywords is empty; the gate would scan nothing")
	}
	want := map[string]bool{}
	for _, kw := range dslclause.StructQueryDirectives {
		want[kw] = true
	}
	for _, kw := range clauseKeywords {
		if !want[kw] {
			t.Errorf("clauseKeywords has %q, which dslclause.StructQueryDirectives does not -- "+
				"the list must not be maintained separately", kw)
		}
	}
	if len(clauseKeywords) != len(dslclause.StructQueryDirectives) {
		t.Errorf("clauseKeywords has %d entries, dslclause.StructQueryDirectives %d",
			len(clauseKeywords), len(dslclause.StructQueryDirectives))
	}
	// `filter` must be in it or the gate misses the shape the issue is about.
	if !want["filter"] {
		t.Error("dslclause.StructQueryDirectives no longer contains `filter`")
	}
}
