package steps

// mutation_control_byte_test.go -- regression guard for memql#3192 on the
// mutation step path, the sibling of render_memql_value_control_byte_test.go.
//
// buildInsertQuery renders the concept, id, parent and aliasOf into the
// statement text with fmt.Sprintf("%q"), and the result goes straight to
// engine.Execute. %q emits `\x00`, `\a` and `\v`; the lexer that re-parses
// this text implements the JSON escapes and only those, so any of them is a
// hard `invalid escape character` at TOKENIZE time -- before the statement is
// a statement at all.
//
// The id is the reachable one: it is an evaluated template (evaluateValue
// handles concat() and friends), so it interpolates prior step results and
// event payload text. A control byte anywhere in that text made the whole
// insert unparseable.

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// mustLex tokenizes through the real lexer -- the layer that rejected the %q
// output -- and returns every string literal it decoded, in order.
func mustLex(t *testing.T, query string) []string {
	t.Helper()
	toks, err := langparser.NewLexer(query).Tokenize()
	if err != nil {
		t.Fatalf("lexer rejected the built statement (the #3192 defect): %v\nquery: %#v", err, query)
	}
	var literals []string
	for _, tok := range toks {
		if tok.Type == langparser.TokenString {
			literals = append(literals, tok.Literal)
		}
	}
	return literals
}

func TestBuildInsertQuery_ControlByteLexes(t *testing.T) {
	e := &MutationExecutor{}

	// A control byte in each %q-rendered position at once.
	const dirtyID = "v1:ship:scan:a\x00b"
	const dirtyParent = "v1:ship:scan:p\ac"
	const dirtyAlias = "v1:ship:scan:a\vd"

	query := e.buildInsertQuery(
		"v1:ship:scan",
		dirtyID,
		map[string]any{"note": "ok"},
		dirtyParent,
		dirtyAlias,
	)

	literals := mustLex(t, query)
	for _, want := range []string{"v1:ship:scan", dirtyID, dirtyParent, dirtyAlias} {
		found := false
		for _, got := range literals {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("literal %#v did not round-trip through the lexer; decoded: %#v", want, literals)
		}
	}
}

// TestBuildInsertQuery_QuotesViaQuoteString pins the single-definition rule:
// the four identifier positions must render through langparser.QuoteString,
// the one definition that lives beside the lexer whose escape set it targets.
func TestBuildInsertQuery_QuotesViaQuoteString(t *testing.T) {
	e := &MutationExecutor{}
	const dirty = "x\x00y"

	query := e.buildInsertQuery(dirty, dirty, nil, dirty, dirty)
	want := langparser.QuoteString(dirty)

	if n := strings.Count(query, want); n != 4 {
		t.Errorf("expected 4 QuoteString-rendered identifiers, found %d in %#v", n, query)
	}
	if strings.Contains(query, `\x00`) {
		t.Errorf("statement still carries a %%q escape the lexer rejects: %#v", query)
	}
}
