package dslclause

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// parseStructQueryBody's switch recognises a directive either as a prefix
// (`strings.HasPrefix(line, "shape")`) or as an exact bare keyword
// (`line == "count"`). Both spellings appear, so both are extracted.
// A trailing space or tab inside the literal (`HasPrefix(line, "limit ")`) is
// tolerated: it is a plausible way to write the same case, and an exact-match
// pattern would miss the directive silently -- which is the failure this guard
// exists to prevent.
//
// KNOWN LIMIT: a case built from a const or variable
// (`HasPrefix(line, kwLimit)`) is invisible to a source scan. That degrades to
// the pre-existing over-inclusive direction rather than to a weakened gate --
// the shared list would simply not terminate on the new directive, as it did
// not before this package existed -- but it is a hole, not a guarantee.
var (
	prefixCaseRe = regexp.MustCompile(`strings\.HasPrefix\(line, "([A-Za-z]+)[ \t]*"\)`)
	exactCaseRe  = regexp.MustCompile(`line == "([A-Za-z]+)[ \t]*"`)
)

// TestStructQueryDirectivesMatchTheParser is the drift guard memql#2815 asked
// for: the shared list must be exactly what parseStructQueryBody accepts.
//
// Without it the two lists drift silently, which is what happened -- the
// conformance walker's copy omitted sort/paginate/asOf/count and emitted 22
// directive lines to the gates as pseudo-predicates. Reading the parser's own
// switch means a directive added there fails HERE until it is added to the
// shared list, rather than being forgotten in one consumer.
//
// This reads source text rather than calling the parser because the switch is
// the specification: there is no exported accessor for "the set of directives
// this body accepts", and inventing one to satisfy a test would just move the
// duplication.
func TestStructQueryDirectivesMatchTheParser(t *testing.T) {
	const rewriterPath = "../parser/rewriter.go"
	raw, err := os.ReadFile(rewriterPath)
	if err != nil {
		t.Fatalf("read %s: %v", rewriterPath, err)
	}

	body := structQueryBodyFunc(t, string(raw))
	found := map[string]bool{}
	for _, m := range prefixCaseRe.FindAllStringSubmatch(body, -1) {
		found[m[1]] = true
	}
	for _, m := range exactCaseRe.FindAllStringSubmatch(body, -1) {
		found[m[1]] = true
	}

	if len(found) == 0 {
		t.Fatal("extracted no directives from parseStructQueryBody; the switch shape changed and this guard has silently stopped protecting anything")
	}

	declared := map[string]bool{}
	for _, kw := range StructQueryDirectives {
		declared[kw] = true
	}

	for kw := range found {
		if !declared[kw] {
			t.Errorf("parseStructQueryBody accepts the directive %q but StructQueryDirectives does not list it; a filter clause would not be terminated by it, and every gate that walks clause text inherits the gap", kw)
		}
	}
	for kw := range declared {
		if !found[kw] {
			t.Errorf("StructQueryDirectives lists %q but parseStructQueryBody does not accept it; the shared list has drifted the other way", kw)
		}
	}

	if t.Failed() {
		t.Logf("parser accepts: %v", sortedKeys(found))
		t.Logf("shared list:    %v", sortedKeys(declared))
	}
}

// structQueryBodyFunc returns the source of parseStructQueryBody, so a
// `HasPrefix(line, ...)` elsewhere in the file cannot be mistaken for a
// struct-query directive.
func structQueryBodyFunc(t *testing.T, src string) string {
	t.Helper()
	const marker = "func parseStructQueryBody("
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("parseStructQueryBody not found in rewriter.go; this guard is keyed on it")
	}
	rest := src[i:]
	// The function ends at the first line that is exactly "}" at column 0.
	if j := strings.Index(rest, "\n}\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestStartsWithRequiresAWordBoundary pins the one place this package
// deliberately differs from the parser. The parser uses a bare HasPrefix, so
// it would read `sortOrder: "asc"` as a `sort` directive. Copying that quirk
// into the gates would silently reclassify a payload field named after a
// directive.
func TestStartsWithRequiresAWordBoundary(t *testing.T) {
	for _, tc := range []struct {
		line, keyword string
		want          bool
	}{
		{"sort", "sort", true},
		{"sort \"row.createdAt\", \"desc\"", "sort", true},
		{"sort\t\"row.createdAt\"", "sort", true},
		{"sortOrder: \"asc\"", "sort", false},
		{"filterKind: \"x\"", "filter", false},
		{"shapeId: 1", "shape", false},
		{"filter row.id==args.x", "filter", true},
		{"", "filter", false},
	} {
		if got := StartsWith(tc.line, tc.keyword); got != tc.want {
			t.Errorf("StartsWith(%q, %q) = %v, want %v", tc.line, tc.keyword, got, tc.want)
		}
	}
}

// TestTerminatesFilterClause covers the keywords a struct-query body does not
// have but a whole-file walker still meets.
func TestTerminatesFilterClause(t *testing.T) {
	terminates := []string{
		"shape spaceFull", "sort \"row.createdAt\"", "paginate 50", "asOf latest",
		"count", "insert {", "update {", "return true", "args {", "use common.traits.{ x }",
		"}", ")", "@description(\"x\")", "",
	}
	for _, line := range terminates {
		if !TerminatesFilterClause(line) {
			t.Errorf("TerminatesFilterClause(%q) = false; a filter clause must not run past it", line)
		}
	}

	continues := []string{
		"row.id==args.x", "&& traitIsActiveRecord", "|| ownerUserId==actor.userId",
		"sortOrder==args.o", "shapeless==true",
	}
	for _, line := range continues {
		if TerminatesFilterClause(line) {
			t.Errorf("TerminatesFilterClause(%q) = true; this is predicate text, not a clause boundary", line)
		}
	}
}
