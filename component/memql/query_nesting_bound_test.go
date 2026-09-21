package memql

// query_nesting_bound_test.go -- the query string a client sends is bounded
// where it is READ.
//
// parseViaLangparser is the engine's entry to the internal query form, and
// three doors reach it with a string a client supplied: ExecuteQueryMsg.query
// on the gRPC stream, the HTTP gateway's POST body, and the MCP `query` tool
// argument. None of them can bound this by SIZE -- a generated SDK call
// inlines its argument values into the same text, and depth costs the same
// frames however few bytes it is written in -- so the bound lives in the
// parser, and this test is the door's own check that it is reached.

import (
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func nestedQuery(levels int) string {
	return `concept=="v1:todos:todo" && ` + strings.Repeat("(", levels) + "row.a==1" + strings.Repeat(")", levels)
}

// Far past the bound: refused, with the rule id, in about the time it takes to
// lex. The plan is never built, so nothing downstream -- the AST converter,
// the filter evaluator, the purity walk -- ever recurses over the tree.
func TestQueryPathRefusesNestingAtCrashDepth(t *testing.T) {
	src := nestedQuery(1_000_000)

	start := time.Now()
	_, err := parseViaLangparser(src, false)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("nesting far past the bound must be refused, not converted into a plan")
	}
	if !strings.Contains(err.Error(), langparser.RuleNestingTooDeep) {
		t.Errorf("the refusal must carry the rule id %q: %v", langparser.RuleNestingTooDeep, err)
	}
	if !strings.Contains(err.Error(), "nests too deeply") {
		t.Errorf("refusal wording: %v", err)
	}
	if elapsed > 30*time.Second {
		t.Errorf("the refusal took %v", elapsed)
	}
	t.Logf("refused %d bytes in %v", len(src), elapsed.Round(time.Millisecond))
}

// Just inside the bound: still a query. Without this the test above would pass
// against a parser that refused everything.
func TestQueryPathParsesJustInsideTheBound(t *testing.T) {
	if _, err := parseViaLangparser(nestedQuery(langparser.MaxNestingDepth-1), false); err != nil {
		t.Fatalf("a query just inside the bound must parse: %v", err)
	}
}

// And the ordinary shapes the engine sees are untouched.
func TestQueryPathStillReadsOrdinaryQueries(t *testing.T) {
	for _, src := range []string{
		`concept=="v1:todos:todo"`,
		`concept=="v1:todos:todo" && (status=="open" || (owner=="u1" && archived==false))`,
		`concept=="v1:todos:todo" && status in ["open", "blocked"]`,
	} {
		if _, err := parseViaLangparser(src, false); err != nil {
			t.Errorf("an ordinary query must still parse: %q\n  %v", src, err)
		}
	}
}
