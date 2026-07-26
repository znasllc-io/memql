package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// clauseAfterBraceRe finds a `filter` or `sort` clause opening LATER ON THE
// SAME LINE as a `{`. The clause keyword must be followed by whitespace or a
// quote so an identifier like `sortOrder` or `filterKind` cannot match.
//
// Two details are load-bearing, and the first revision got both wrong:
//
//   - `.*?` rather than `[^{}]*?`. A negated-brace class cannot cross a
//     nested body, so `query w q { args { x string } filter id == args.x }`
//     -- the most plausible way an author actually writes this -- hid the
//     clause behind the inner `{}` and sailed through.
//   - `[\s\p{Zs}"]` rather than `[ \t"]`. Go's `\s` is ASCII-only, so an
//     NBSP between the keyword and its argument slipped the gate. That is
//     the exact assumption the sibling scanner rejects in
//     row_intrinsic_scan.go (isSortClauseOpener: "An ASCII-only TrimLeft
//     here would let it bypass the gate"), reintroduced one file over.
var clauseAfterBraceRe = regexp.MustCompile(`\{.*?\b(filter|sort)\b[\s\p{Zs}"]`)

// TestClauseDoesNotShareTheOpeningBraceLine keeps the row-intrinsic gates'
// line contract true by construction (memql#2817).
//
// Both scanners in component/memql/sense are LINE-oriented: they scan a line
// only when it OPENS the clause (isFilterClauseOpener / isSortClauseOpener).
// A struct query whose clause sits on the same line as its opening brace is
// therefore invisible to them, and the language accepts that shape --
// `query widget q { sort "createdAt", "desc" }` normalises cleanly. Measured
// before this gate:
//
//	query widget q { sort "createdAt", "desc" }   sortHits=0   filterHits=0
//	query widget q { filter id == args.x }        sortHits=0   filterHits=0
//	query widget q {
//	  sort "createdAt", "desc"                    sortHits=1
//
// So a bare intrinsic authored that way compiles, loads, and passes BOTH
// TestFilterIntrinsicsUseRowNamespace (#2779) and TestSortKeysUseRowNamespace
// (#2786) -- the exact silent failure those gates exist to prevent. A bare
// sort key orders on a JSONB path no row carries (a no-op sort, not an error);
// a bare filter intrinsic compiles to a different predicate than intended.
//
// WHY A GATE RATHER THAN A SMARTER SCANNER. The scanners are deliberately
// line-oriented, and row_intrinsic_scan.go records why multi-line tracking was
// REMOVED: it gave block comments a way to escape the single-line bound and it
// protected a grammar the parser rejects. Rebuilding it would reintroduce
// that. Normalising the source before scanning is the other option, but the
// scanners report Line/Column that editors underline from, and rewriting the
// text to scan it would invalidate exactly those positions.
//
// Pinning the invariant the scanners already assume is cheaper than either,
// and it fails at a precise file:line the author can act on.
//
// Scope is the two clause keywords the scanners walk, NOT one-line construct
// bodies in general: dsl/rbac/seeds.memql writes 60 one-line `seed` rows on
// purpose, and a seed body carries field assignments, never a filter or sort.
func TestClauseDoesNotShareTheOpeningBraceLine(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	violations := 0
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}

		for i, line := range strings.Split(blankBlockComments(string(raw)), "\n") {
			code := blankQuotedAndComments(line)
			m := clauseAfterBraceRe.FindStringSubmatch(code)
			if m == nil {
				continue
			}
			violations++
			t.Errorf("%s:%d  a `%s` clause shares the line with its opening brace, where the row-intrinsic scanners cannot see it -- put the clause on its own line.\n    %s",
				p, i+1, m[1], strings.TrimSpace(line))
		}
	}
	if violations > 0 {
		t.Errorf("found %d clause(s) sharing an opening-brace line; both row-intrinsic gates (#2779, #2786) are blind to them (memql#2817)", violations)
	}
}

// TestClauseAfterBraceDetector pins the detector itself. The corpus satisfies
// the rule today, so the corpus alone cannot show the gate works -- and a
// detector that matched nothing would pass just as quietly.
func TestClauseAfterBraceDetector(t *testing.T) {
	shouldMatch := []string{
		`query widget q { sort "createdAt", "desc" }`,
		`query widget q { filter id == args.x }`,
		`query widget q { sort "createdAt"`,
		`query widget q {  filter  id==args.x`,
		`query widget q {	sort	"createdAt"`,
		// Review round 1: a nested body on the same line hid the clause from
		// a negated-brace class. The most plausible authoring shape of the set.
		`query widget q { args { x string } filter id == args.x }`,
		`query widget q { args { x string } sort "createdAt","desc" }`,
		// Review round 1: an NBSP between the keyword and its argument slipped
		// an ASCII-only separator class -- the same assumption the sibling
		// scanner explicitly rejects one file over.
		"query widget q { sort \"createdAt\",\"desc\" }",
		"query widget q { filter id == args.x }",
	}
	for _, s := range shouldMatch {
		if !clauseAfterBraceRe.MatchString(blankQuotedAndComments(s)) {
			t.Errorf("clauseAfterBraceRe did not match %q; this is the shape the gate exists for", s)
		}
	}

	shouldNotMatch := []string{
		// The canonical form: clause on its own line.
		`  sort    "row.createdAt", "desc"`,
		`  filter  row.id==args.x`,
		`query widget q {`,
		// Identifiers that merely begin with a clause keyword.
		`query widget q { sortOrder: "asc" }`,
		`query widget q { filterKind: "x" }`,
		// A one-line seed body -- 60 of these ship on purpose, and a seed
		// carries field assignments, never a filter or sort clause.
		`seed capability cap-owner-read { roleSlug: "owner"  verb: "read" }`,
		// The words inside a string or a comment are not clauses.
		`  x string @description("use filter to narrow")`,
		`// query widget q { sort "createdAt" }`,
	}
	for _, s := range shouldNotMatch {
		if clauseAfterBraceRe.MatchString(blankQuotedAndComments(s)) {
			t.Errorf("clauseAfterBraceRe matched %q; only a real clause sharing the brace line may be flagged", s)
		}
	}
}
